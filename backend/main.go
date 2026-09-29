package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

const (
	serverAddr = ":8080"
)

func main() {
	applicationConfig, err := LoadConfig()
	if err != nil {
		log.Fatalf("application configuration is invalid: %v", err)
	}

	database, err := OpenDatabase(applicationConfig.Database)
	if err != nil {
		log.Fatalf("database initialization failed: %v", err)
	}
	if err := CheckDatabase(context.Background(), database, applicationConfig.Database.PingTimeout); err != nil {
		_ = database.Close()
		log.Fatalf("database health check failed: %v", err)
	}

	userRepository, err := NewPostgresUserRepository(database)
	if err != nil {
		_ = database.Close()
		log.Fatalf("user repository initialization failed: %v", err)
	}
	userService, err := NewUserService(userRepository)
	if err != nil {
		_ = database.Close()
		log.Fatalf("user service initialization failed: %v", err)
	}
	jwtService, err := NewJWTService(applicationConfig.JWT)
	if err != nil {
		_ = database.Close()
		log.Fatalf("JWT service initialization failed: %v", err)
	}
	authHandler, err := NewAuthHandler(userService, jwtService)
	if err != nil {
		_ = database.Close()
		log.Fatalf("authentication handler initialization failed: %v", err)
	}

	kubernetesConfig := LoadKubernetesConfig()
	kubernetesClient, err := NewKubernetesClient(kubernetesConfig)
	if err != nil {
		_ = database.Close()
		log.Fatalf("Kubernetes client initialization failed: %v", err)
	}
	labRuntime, err := NewKubernetesRuntime(kubernetesClient, KubernetesRuntimeConfig{Namespace: kubernetesClient.Namespace})
	if err != nil {
		_ = database.Close()
		log.Fatalf("lab runtime initialization failed: %v", err)
	}
	sessionManager, err := NewSessionManager(labRuntime)
	if err != nil {
		_ = database.Close()
		log.Fatalf("session manager initialization failed: %v", err)
	}
	labSessionRepository, err := NewLabSessionRepository(database)
	if err != nil {
		_ = database.Close()
		log.Fatalf("lab session repository initialization failed: %v", err)
	}
	labSessionService, err := NewLabSessionService(labSessionRepository, sessionManager)
	if err != nil {
		_ = database.Close()
		log.Fatalf("lab session service initialization failed: %v", err)
	}
	labSessionHandler, err := NewLabSessionHandler(labSessionService)
	if err != nil {
		_ = database.Close()
		log.Fatalf("lab session handler initialization failed: %v", err)
	}
	terminalHandler, err := NewTerminalHandler(labSessionService, labRuntime, NewTerminalTicketStore(), applicationConfig.FrontendOrigins)
	if err != nil {
		_ = database.Close()
		log.Fatalf("terminal handler initialization failed: %v", err)
	}

	defer database.Close()

	// Serve frontend
	http.Handle(
		"/",
		http.FileServer(http.Dir("./frontend")),
	)

	http.HandleFunc(
		"/health/kubernetes",
		kubernetesHealthHandler(kubernetesClient),
	)
	if err := RegisterAuthRoutes(http.DefaultServeMux, authHandler, jwtService); err != nil {
		log.Fatalf("authentication route registration failed: %v", err)
	}
	if err := RegisterLabSessionRoutes(http.DefaultServeMux, labSessionHandler, jwtService); err != nil {
		log.Fatalf("lab session route registration failed: %v", err)
	}
	if err := terminalHandler.RegisterTicketRoute(http.DefaultServeMux, labSessionHandler); err != nil {
		log.Fatalf("terminal route registration failed: %v", err)
	}

	log.Printf(
		"Cyber Lab running at http://localhost%s",
		serverAddr,
	)

	// Graceful shutdown
	server := &http.Server{
		Addr: serverAddr,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil &&
			err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sigChan := make(chan os.Signal, 1)

	signal.Notify(
		sigChan,
		syscall.SIGINT,
		syscall.SIGTERM,
	)

	<-sigChan

	log.Println("Cyber Lab shutting down...")
}
