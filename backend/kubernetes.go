package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	kubeconfigEnv    = "CYBERLAB_KUBECONFIG"
	namespaceEnv     = "CYBERLAB_NAMESPACE"
	defaultNamespace = "cyber-labs"
)

type KubernetesConfig struct {
	KubeconfigPath string
	Namespace      string
}

type KubernetesClient struct {
	Clientset  kubernetes.Interface
	Namespace  string
	RESTConfig *rest.Config
}

func LoadKubernetesConfig() KubernetesConfig {
	namespace := strings.TrimSpace(os.Getenv(namespaceEnv))
	if namespace == "" {
		namespace = defaultNamespace
	}

	return KubernetesConfig{
		KubeconfigPath: strings.TrimSpace(os.Getenv(kubeconfigEnv)),
		Namespace:      namespace,
	}
}

func NewKubernetesClient(config KubernetesConfig) (*KubernetesClient, error) {
	return newKubernetesClientWithLoaders(
		config,
		func(path string) (*rest.Config, error) {
			return clientcmd.BuildConfigFromFlags("", path)
		},
		rest.InClusterConfig,
	)
}

func newKubernetesClientWithLoaders(
	config KubernetesConfig,
	kubeconfigLoader func(string) (*rest.Config, error),
	inClusterLoader func() (*rest.Config, error),
) (*KubernetesClient, error) {
	if strings.TrimSpace(config.Namespace) == "" {
		return nil, errors.New("Kubernetes namespace is required")
	}

	var (
		restConfig *rest.Config
		err        error
	)

	if config.KubeconfigPath != "" {
		restConfig, err = kubeconfigLoader(config.KubeconfigPath)
	} else {
		restConfig, err = inClusterLoader()
	}
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return &KubernetesClient{
		Clientset:  clientset,
		Namespace:  config.Namespace,
		RESTConfig: restConfig,
	}, nil
}

func (client *KubernetesClient) Check() error {
	_, err := client.Clientset.Discovery().ServerVersion()
	return err
}

func kubernetesHealthHandler(client *KubernetesClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := client.Check(); err != nil {
			log.Printf("Kubernetes health check failed: %v", err)
			writeKubernetesHealth(w, http.StatusServiceUnavailable, "unavailable")
			return
		}

		writeKubernetesHealth(w, http.StatusOK, "ok")
	}
}

func writeKubernetesHealth(w http.ResponseWriter, status int, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": state})
}
