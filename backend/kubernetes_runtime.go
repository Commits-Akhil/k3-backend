package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	defaultKaliImage           = "lukaszlach/kali-desktop:xfce"
	defaultCPURequest          = "100m"
	defaultMemoryRequest       = "256Mi"
	defaultCPULimit            = "500m"
	defaultMemoryLimit         = "768Mi"
	containerPort        int32 = 6080

	appLabel      = "app.kubernetes.io/name"
	appLabelValue = "cyberlab"
	sessionLabel  = "cyberlab.io/session-id"
	labLabel      = "cyberlab.io/lab-id"
)

type KubernetesRuntimeConfig struct {
	Namespace     string
	Image         string
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
}

type KubernetesRuntime struct {
	client    *KubernetesClient
	config    KubernetesRuntimeConfig
	resources corev1.ResourceRequirements
}

func NewKubernetesRuntime(client *KubernetesClient, config KubernetesRuntimeConfig) (*KubernetesRuntime, error) {
	if client == nil || client.Clientset == nil {
		return nil, errors.New("Kubernetes client is required")
	}
	if config.Namespace == "" {
		config.Namespace = client.Namespace
	}
	if config.Namespace == "" {
		return nil, errors.New("Kubernetes namespace is required")
	}
	if config.Image == "" {
		config.Image = defaultKaliImage
	}
	if config.CPURequest == "" {
		config.CPURequest = defaultCPURequest
	}
	if config.MemoryRequest == "" {
		config.MemoryRequest = defaultMemoryRequest
	}
	if config.CPULimit == "" {
		config.CPULimit = defaultCPULimit
	}
	if config.MemoryLimit == "" {
		config.MemoryLimit = defaultMemoryLimit
	}

	requests, err := parseQuantities(config.CPURequest, config.MemoryRequest)
	if err != nil {
		return nil, fmt.Errorf("parse resource requests: %w", err)
	}
	limits, err := parseQuantities(config.CPULimit, config.MemoryLimit)
	if err != nil {
		return nil, fmt.Errorf("parse resource limits: %w", err)
	}

	return &KubernetesRuntime{
		client: client,
		config: config,
		resources: corev1.ResourceRequirements{
			Requests: requests,
			Limits:   limits,
		},
	}, nil
}

func (runtime *KubernetesRuntime) Create(ctx context.Context, request RuntimeCreateRequest) (RuntimeReference, error) {
	if err := validateSessionID(request.SessionID); err != nil {
		return RuntimeReference{}, err
	}
	if err := validateLabIdentifier(request.LabID); err != nil {
		return RuntimeReference{}, err
	}

	names := runtimeNames(request.SessionID)
	resourceLabels := runtimeLabels(request.SessionID, request.LabID)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.pod,
			Namespace: runtime.config.Namespace,
			Labels:    resourceLabels,
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: boolPointer(false),
			RestartPolicy:                corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "kali",
				Image:           runtime.config.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports: []corev1.ContainerPort{{
					Name:          "gui",
					ContainerPort: containerPort,
				}},
				Resources: runtime.resources,
			}},
		},
	}

	if _, err := runtime.client.Clientset.CoreV1().Pods(runtime.config.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return RuntimeReference{}, fmt.Errorf("create lab Pod: %w", err)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.service,
			Namespace: runtime.config.Namespace,
			Labels:    resourceLabels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: resourceLabels,
			Ports: []corev1.ServicePort{{
				Name:       "gui",
				Port:       containerPort,
				TargetPort: intstrFromInt(containerPort),
			}},
		},
	}

	if _, err := runtime.client.Clientset.CoreV1().Services(runtime.config.Namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		cleanupErr := runtime.deleteOwnedPod(ctx, names.pod, resourceLabels)
		if cleanupErr != nil {
			return RuntimeReference{}, errors.Join(fmt.Errorf("create lab Service: %w", err), cleanupErr)
		}
		return RuntimeReference{}, fmt.Errorf("create lab Service: %w", err)
	}

	return RuntimeReference{
		Namespace:   runtime.config.Namespace,
		PodName:     names.pod,
		ServiceName: names.service,
		Labels:      cloneLabels(resourceLabels),
	}, nil
}

func (runtime *KubernetesRuntime) Status(ctx context.Context, reference RuntimeReference) (RuntimeStatus, error) {
	if err := runtime.validateReference(reference); err != nil {
		return RuntimeStatus{}, err
	}

	pod, podErr := runtime.client.Clientset.CoreV1().Pods(reference.Namespace).Get(ctx, reference.PodName, metav1.GetOptions{})
	service, serviceErr := runtime.client.Clientset.CoreV1().Services(reference.Namespace).Get(ctx, reference.ServiceName, metav1.GetOptions{})
	podMissing := apierrors.IsNotFound(podErr)
	serviceMissing := apierrors.IsNotFound(serviceErr)
	if podErr != nil && !podMissing {
		return RuntimeStatus{}, fmt.Errorf("get lab Pod: %w", podErr)
	}
	if serviceErr != nil && !serviceMissing {
		return RuntimeStatus{}, fmt.Errorf("get lab Service: %w", serviceErr)
	}
	if podMissing && serviceMissing {
		return RuntimeStatus{Phase: RuntimeMissing}, nil
	}
	if podMissing || serviceMissing {
		return RuntimeStatus{Phase: RuntimeOrphaned}, nil
	}
	if !ownedByReference(pod.Labels, reference.Labels) || !ownedByReference(service.Labels, reference.Labels) {
		return RuntimeStatus{}, errors.New("lab Pod ownership labels do not match")
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
		return RuntimeStatus{Phase: RuntimeReady, Ready: podReady(pod)}, nil
	case corev1.PodSucceeded:
		return RuntimeStatus{Phase: RuntimeStopped}, nil
	case corev1.PodFailed:
		return RuntimeStatus{Phase: RuntimeFailed}, nil
	default:
		return RuntimeStatus{Phase: RuntimeStarting}, nil
	}
}

func (runtime *KubernetesRuntime) Stop(ctx context.Context, reference RuntimeReference) error {
	if err := runtime.validateReference(reference); err != nil {
		return err
	}

	var cleanupErrors []error
	if err := runtime.deleteOwnedService(ctx, reference.ServiceName, reference.Labels); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := runtime.deleteOwnedPod(ctx, reference.PodName, reference.Labels); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func (runtime *KubernetesRuntime) StreamTerminal(ctx context.Context, reference RuntimeReference, input io.Reader, output io.Writer, sizes <-chan TerminalSize) error {
	if runtime.client.RESTConfig == nil {
		return errors.New("Kubernetes streaming configuration is unavailable")
	}
	if err := runtime.validateReference(reference); err != nil {
		return err
	}
	request := runtime.client.Clientset.CoreV1().RESTClient().Post().
		Namespace(reference.Namespace).
		Resource("pods").
		Name(reference.PodName).
		SubResource("exec")
	request.VersionedParams(&corev1.PodExecOptions{
		Container: "kali",
		Command:   []string{"bash"},
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(runtime.client.RESTConfig, http.MethodPost, request.URL())
	if err != nil {
		return errors.New("create Kubernetes terminal stream failed")
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             input,
		Stdout:            output,
		Stderr:            output,
		Tty:               true,
		TerminalSizeQueue: &terminalSizeQueue{context: ctx, sizes: sizes},
	})
}

type terminalSizeQueue struct {
	context context.Context
	sizes   <-chan TerminalSize
}

func (queue *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case <-queue.context.Done():
		return nil
	case size, ok := <-queue.sizes:
		if !ok {
			return nil
		}
		return &remotecommand.TerminalSize{Width: size.Cols, Height: size.Rows}
	}
}

type runtimeNamesResult struct {
	pod     string
	service string
}

func runtimeNames(sessionID string) runtimeNamesResult {
	return runtimeNamesResult{
		pod:     "cyberlab-pod-" + sessionID,
		service: "cyberlab-svc-" + sessionID,
	}
}

func runtimeLabels(sessionID, labID string) map[string]string {
	return map[string]string{
		appLabel:     appLabelValue,
		sessionLabel: sessionID,
		labLabel:     labID,
	}
}

func (runtime *KubernetesRuntime) validateReference(reference RuntimeReference) error {
	if reference.Namespace != runtime.config.Namespace || reference.PodName == "" || reference.ServiceName == "" {
		return errors.New("invalid lab runtime reference")
	}
	sessionID := reference.Labels[sessionLabel]
	labID := reference.Labels[labLabel]
	if err := validateSessionID(sessionID); err != nil {
		return errors.New("invalid lab runtime labels")
	}
	if err := validateLabIdentifier(labID); err != nil {
		return errors.New("invalid lab runtime labels")
	}
	names := runtimeNames(sessionID)
	if reference.PodName != names.pod || reference.ServiceName != names.service {
		return errors.New("lab runtime names do not match session labels")
	}
	if reference.Labels[appLabel] != appLabelValue {
		return errors.New("invalid lab runtime labels")
	}
	return nil
}

func (runtime *KubernetesRuntime) deleteOwnedService(ctx context.Context, name string, expectedLabels map[string]string) error {
	services := runtime.client.Clientset.CoreV1().Services(runtime.config.Namespace)
	service, err := services.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get lab Service: %w", err)
	}
	if !ownedByReference(service.Labels, expectedLabels) {
		return errors.New("lab Service ownership labels do not match")
	}
	if err := services.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete lab Service: %w", err)
	}
	return nil
}

func (runtime *KubernetesRuntime) deleteOwnedPod(ctx context.Context, name string, expectedLabels map[string]string) error {
	pods := runtime.client.Clientset.CoreV1().Pods(runtime.config.Namespace)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get lab Pod: %w", err)
	}
	if !ownedByReference(pod.Labels, expectedLabels) {
		return errors.New("lab Pod ownership labels do not match")
	}
	if err := pods.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete lab Pod: %w", err)
	}
	return nil
}

func ownedByReference(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func parseQuantities(cpu, memory string) (corev1.ResourceList, error) {
	cpuQuantity, err := resource.ParseQuantity(cpu)
	if err != nil {
		return nil, err
	}
	memoryQuantity, err := resource.ParseQuantity(memory)
	if err != nil {
		return nil, err
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    cpuQuantity,
		corev1.ResourceMemory: memoryQuantity,
	}, nil
}

func validateSessionID(sessionID string) error {
	if len(validation.IsDNS1123Label(sessionID)) != 0 {
		return errors.New("invalid session ID")
	}
	return nil
}

func boolPointer(value bool) *bool {
	return &value
}

func intstrFromInt(value int32) intstr.IntOrString {
	return intstr.FromInt32(value)
}

func cloneLabels(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
