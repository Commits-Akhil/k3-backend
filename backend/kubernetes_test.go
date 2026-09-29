package main

import (
	"errors"
	"testing"

	"k8s.io/client-go/rest"
)

func TestLoadKubernetesConfigUsesEnvironmentAndDefaultNamespace(t *testing.T) {
	t.Setenv(kubeconfigEnv, "/tmp/restricted-kubeconfig")
	t.Setenv(namespaceEnv, "")

	config := LoadKubernetesConfig()

	if config.KubeconfigPath != "/tmp/restricted-kubeconfig" {
		t.Fatalf("kubeconfig path = %q, want explicit path", config.KubeconfigPath)
	}
	if config.Namespace != defaultNamespace {
		t.Fatalf("namespace = %q, want %q", config.Namespace, defaultNamespace)
	}
}

func TestNewKubernetesClientUsesExplicitKubeconfig(t *testing.T) {
	var kubeconfigPath string
	inClusterCalled := false

	client, err := newKubernetesClientWithLoaders(
		KubernetesConfig{
			KubeconfigPath: "/tmp/restricted-kubeconfig",
			Namespace:      "cyber-labs",
		},
		func(path string) (*rest.Config, error) {
			kubeconfigPath = path
			return &rest.Config{Host: "https://kubernetes.invalid"}, nil
		},
		func() (*rest.Config, error) {
			inClusterCalled = true
			return nil, errors.New("in-cluster loader should not be called")
		},
	)

	if err != nil {
		t.Fatalf("NewKubernetesClient returned error: %v", err)
	}
	if client.Namespace != "cyber-labs" {
		t.Fatalf("namespace = %q, want %q", client.Namespace, "cyber-labs")
	}
	if kubeconfigPath != "/tmp/restricted-kubeconfig" {
		t.Fatalf("kubeconfig path = %q, want explicit path", kubeconfigPath)
	}
	if inClusterCalled {
		t.Fatal("in-cluster loader was called for explicit kubeconfig")
	}
}

func TestNewKubernetesClientUsesInClusterConfigWhenKubeconfigIsUnset(t *testing.T) {
	inClusterCalled := false

	client, err := newKubernetesClientWithLoaders(
		KubernetesConfig{Namespace: "cyber-labs"},
		func(string) (*rest.Config, error) {
			return nil, errors.New("kubeconfig loader should not be called")
		},
		func() (*rest.Config, error) {
			inClusterCalled = true
			return &rest.Config{Host: "https://kubernetes.invalid"}, nil
		},
	)

	if err != nil {
		t.Fatalf("NewKubernetesClient returned error: %v", err)
	}
	if !inClusterCalled {
		t.Fatal("in-cluster loader was not called")
	}
	if client.Namespace != "cyber-labs" {
		t.Fatalf("namespace = %q, want %q", client.Namespace, "cyber-labs")
	}
}

func TestNewKubernetesClientRequiresNamespace(t *testing.T) {
	_, err := newKubernetesClientWithLoaders(
		KubernetesConfig{},
		func(string) (*rest.Config, error) { return nil, nil },
		func() (*rest.Config, error) { return nil, nil },
	)

	if err == nil {
		t.Fatal("NewKubernetesClient succeeded without a namespace")
	}
}
