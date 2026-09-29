package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func newFakeRuntime(t *testing.T) (*KubernetesRuntime, *fake.Clientset) {
	t.Helper()

	clientset := fake.NewSimpleClientset()
	client, err := NewKubernetesRuntime(
		&KubernetesClient{Clientset: clientset, Namespace: defaultNamespace},
		KubernetesRuntimeConfig{},
	)
	if err != nil {
		t.Fatalf("NewKubernetesRuntime returned error: %v", err)
	}
	return client, clientset
}

func TestKubernetesRuntimeCreatesExpectedResources(t *testing.T) {
	runtimeClient, clientset := newFakeRuntime(t)

	reference, err := runtimeClient.Create(context.Background(), RuntimeCreateRequest{
		SessionID: "s-0123456789abcdef0123456789abcdef",
		LabID:     "intro-linux",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	pod, err := clientset.CoreV1().Pods(defaultNamespace).Get(context.Background(), reference.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created Pod: %v", err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("Pod ServiceAccount token automount is not disabled")
	}
	if pod.Spec.Containers[0].Image != defaultKaliImage {
		t.Fatalf("Pod image = %q, want %q", pod.Spec.Containers[0].Image, defaultKaliImage)
	}
	cpuRequest := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if got := cpuRequest.String(); got != defaultCPURequest {
		t.Fatalf("CPU request = %q, want %q", got, defaultCPURequest)
	}
	memoryRequest := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	if got := memoryRequest.String(); got != defaultMemoryRequest {
		t.Fatalf("memory request = %q, want %q", got, defaultMemoryRequest)
	}
	cpuLimit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	if got := cpuLimit.String(); got != defaultCPULimit {
		t.Fatalf("CPU limit = %q, want %q", got, defaultCPULimit)
	}
	memoryLimit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	if got := memoryLimit.String(); got != defaultMemoryLimit {
		t.Fatalf("memory limit = %q, want %q", got, defaultMemoryLimit)
	}

	service, err := clientset.CoreV1().Services(defaultNamespace).Get(context.Background(), reference.ServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created Service: %v", err)
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("Service type = %q, want ClusterIP", service.Spec.Type)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != containerPort {
		t.Fatalf("Service ports = %#v, want port %d", service.Spec.Ports, containerPort)
	}
	if !ownedByReference(service.Spec.Selector, reference.Labels) {
		t.Fatal("Service selector does not match session labels")
	}
	if !ownedByReference(pod.Labels, reference.Labels) {
		t.Fatal("Pod labels do not match session labels")
	}
}

func TestKubernetesRuntimeReportsReadyOnlyForReadyPod(t *testing.T) {
	runtimeClient, clientset := newFakeRuntime(t)
	reference, err := runtimeClient.Create(context.Background(), RuntimeCreateRequest{
		SessionID: "s-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		LabID:     "intro-linux",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	pod, _ := clientset.CoreV1().Pods(defaultNamespace).Get(context.Background(), reference.PodName, metav1.GetOptions{})
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	_, err = clientset.CoreV1().Pods(defaultNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update non-ready Pod: %v", err)
	}

	status, err := runtimeClient.Status(context.Background(), reference)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Phase != RuntimeReady || status.Ready {
		t.Fatalf("status = %#v, want running but not ready", status)
	}

	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	_, err = clientset.CoreV1().Pods(defaultNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update ready Pod: %v", err)
	}
	status, err = runtimeClient.Status(context.Background(), reference)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Phase != RuntimeReady || !status.Ready {
		t.Fatalf("status = %#v, want ready", status)
	}
}

func TestKubernetesRuntimeReportsOrphanedResources(t *testing.T) {
	runtimeClient, clientset := newFakeRuntime(t)
	reference, err := runtimeClient.Create(context.Background(), RuntimeCreateRequest{
		SessionID: "s-dddddddddddddddddddddddddddddddd",
		LabID:     "intro-linux",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := clientset.CoreV1().Services(defaultNamespace).Delete(context.Background(), reference.ServiceName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete Service: %v", err)
	}
	status, err := runtimeClient.Status(context.Background(), reference)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Phase != RuntimeOrphaned {
		t.Fatalf("status phase = %q, want %q", status.Phase, RuntimeOrphaned)
	}
}

func TestKubernetesRuntimeCleansPodWhenServiceCreationFails(t *testing.T) {
	runtimeClient, clientset := newFakeRuntime(t)
	clientset.PrependReactor("create", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("service create failed")
	})

	_, err := runtimeClient.Create(context.Background(), RuntimeCreateRequest{
		SessionID: "s-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		LabID:     "intro-linux",
	})
	if err == nil {
		t.Fatal("Create succeeded despite Service failure")
	}

	names := runtimeNames("s-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	_, getErr := clientset.CoreV1().Pods(defaultNamespace).Get(context.Background(), names.pod, metav1.GetOptions{})
	if !apierrors.IsNotFound(getErr) {
		t.Fatalf("created Pod still exists, get error = %v", getErr)
	}
}

func TestKubernetesRuntimeStopRequiresMatchingLabelsAndIsIdempotent(t *testing.T) {
	runtimeClient, clientset := newFakeRuntime(t)
	reference, err := runtimeClient.Create(context.Background(), RuntimeCreateRequest{
		SessionID: "s-cccccccccccccccccccccccccccccccc",
		LabID:     "intro-linux",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	service, _ := clientset.CoreV1().Services(defaultNamespace).Get(context.Background(), reference.ServiceName, metav1.GetOptions{})
	service.Labels[sessionLabel] = "another-session"
	_, err = clientset.CoreV1().Services(defaultNamespace).Update(context.Background(), service, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update Service labels: %v", err)
	}

	if err := runtimeClient.Stop(context.Background(), reference); err == nil {
		t.Fatal("Stop succeeded despite mismatched Service labels")
	}
	if _, err := clientset.CoreV1().Services(defaultNamespace).Get(context.Background(), reference.ServiceName, metav1.GetOptions{}); err != nil {
		t.Fatalf("mismatched Service should remain after protected cleanup: %v", err)
	}

	service.Labels = cloneLabels(reference.Labels)
	_, err = clientset.CoreV1().Services(defaultNamespace).Update(context.Background(), service, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("restore Service labels: %v", err)
	}
	if err := runtimeClient.Stop(context.Background(), reference); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if err := runtimeClient.Stop(context.Background(), reference); err != nil {
		t.Fatalf("second Stop returned error: %v", err)
	}
}

func TestKubernetesRuntimeRejectsReferenceWithMismatchedIdentity(t *testing.T) {
	runtimeClient, _ := newFakeRuntime(t)

	err := runtimeClient.Stop(context.Background(), RuntimeReference{
		Namespace:   defaultNamespace,
		PodName:     "cyberlab-pod-s-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ServiceName: "cyberlab-svc-s-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Labels:      runtimeLabels("s-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "intro-linux"),
	})
	if err == nil {
		t.Fatal("Stop accepted a reference whose names do not match its labels")
	}
}

func TestRuntimeNamesAreSafe(t *testing.T) {
	names := runtimeNames("s-0123456789abcdef0123456789abcdef")
	if len(names.pod) > 63 || len(names.service) > 63 {
		t.Fatalf("resource names exceed Kubernetes label length: %#v", names)
	}
	if err := validateSessionID("not safe!"); err == nil {
		t.Fatal("invalid session ID was accepted")
	}
}

type fakeLabRuntime struct {
	mu        sync.Mutex
	created   map[string]RuntimeReference
	stopped   int
	createErr error
}

func (runtime *fakeLabRuntime) Create(_ context.Context, request RuntimeCreateRequest) (RuntimeReference, error) {
	if runtime.createErr != nil {
		return RuntimeReference{}, runtime.createErr
	}
	reference := RuntimeReference{
		Namespace:   defaultNamespace,
		PodName:     "pod-" + request.SessionID,
		ServiceName: "service-" + request.SessionID,
		Labels:      runtimeLabels(request.SessionID, request.LabID),
	}
	runtime.mu.Lock()
	if runtime.created == nil {
		runtime.created = make(map[string]RuntimeReference)
	}
	runtime.created[request.SessionID] = reference
	runtime.mu.Unlock()
	return reference, nil
}

func (runtime *fakeLabRuntime) Status(_ context.Context, _ RuntimeReference) (RuntimeStatus, error) {
	return RuntimeStatus{Phase: RuntimeReady, Ready: true}, nil
}

func (runtime *fakeLabRuntime) Stop(_ context.Context, _ RuntimeReference) error {
	runtime.mu.Lock()
	runtime.stopped++
	runtime.mu.Unlock()
	return nil
}
