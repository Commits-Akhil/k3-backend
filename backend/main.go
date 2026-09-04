package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

const (
	serverAddr    = ":8080"
	sshHost       = "192.168.29.123:22"
	sshUser       = "akhil"
	containerName = "lukas-kali-test"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type ClientMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func main() {
	// Serve frontend
	http.Handle(
		"/",
		http.FileServer(http.Dir("./frontend")),
	)

	// Terminal WebSocket
	http.HandleFunc(
		"/api/terminal",
		terminalHandler,
	)

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

// ======================================================
// TERMINAL WEBSOCKET
// ======================================================

func terminalHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	conn, err := upgrader.Upgrade(
		w,
		r,
		nil,
	)

	if err != nil {
		log.Println(
			"WebSocket upgrade error:",
			err,
		)
		return
	}

	defer conn.Close()

	// ==================================================
	// SSH PASSWORD
	// ==================================================

	password := os.Getenv(
		"MINISHIELD_SSH_PASSWORD",
	)

	if password == "" {
		_ = conn.WriteMessage(
			websocket.TextMessage,
			[]byte(
				"MINISHIELD_SSH_PASSWORD is not set\r\n",
			),
		)
		return
	}

	// ==================================================
	// SSH CONFIG
	// ==================================================

	sshConfig := &ssh.ClientConfig{
		User: sshUser,

		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},

		// Prototype only.
		// Production should verify the VM host key.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// ==================================================
	// CONNECT TO VM 101
	// ==================================================

	client, err := ssh.Dial(
		"tcp",
		sshHost,
		sshConfig,
	)

	if err != nil {
		log.Println(
			"SSH connection failed:",
			err,
		)

		_ = conn.WriteMessage(
			websocket.TextMessage,
			[]byte(
				fmt.Sprintf(
					"\r\nSSH connection failed: %v\r\n",
					err,
				),
			),
		)

		return
	}

	defer client.Close()

	// ==================================================
	// CREATE SSH SESSION
	// ==================================================

	session, err := client.NewSession()

	if err != nil {
		log.Println(
			"SSH session creation failed:",
			err,
		)
		return
	}

	defer session.Close()

	// ==================================================
	// CREATE REMOTE PTY
	// ==================================================

	termModes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	err = session.RequestPty(
		"xterm-256color",
		30,
		120,
		termModes,
	)

	if err != nil {
		log.Println(
			"PTY request failed:",
			err,
		)
		return
	}

	// ==================================================
	// SSH STDIN
	// ==================================================

	stdin, err := session.StdinPipe()

	if err != nil {
		log.Println(
			"stdin pipe error:",
			err,
		)
		return
	}

	// ==================================================
	// SSH OUTPUT
	// ==================================================

	outputReader, outputWriter := io.Pipe()

	session.Stdout = outputWriter
	session.Stderr = outputWriter

	// ==================================================
	// START REAL KALI SHELL
	// ==================================================

	command := fmt.Sprintf(
		"docker exec -it %s bash",
		containerName,
	)

	log.Println(
		"Starting:",
		command,
	)

	err = session.Start(command)

	if err != nil {
		log.Println(
			"docker exec start failed:",
			err,
		)

		_ = outputWriter.Close()

		return
	}

	// ==================================================
	// SSH OUTPUT → BROWSER
	// ==================================================

	outputDone := make(chan struct{})

	go func() {
		defer close(outputDone)

		buffer := make([]byte, 32*1024)

		for {
			n, readErr := outputReader.Read(buffer)

			if n > 0 {
				data := make([]byte, n)

				copy(
					data,
					buffer[:n],
				)

				writeErr := conn.WriteMessage(
					websocket.BinaryMessage,
					data,
				)

				if writeErr != nil {
					return
				}
			}

			if readErr != nil {
				return
			}
		}
	}()

	// ==================================================
	// BROWSER → SSH
	// ==================================================

	for {
		messageType, messageData, readErr :=
			conn.ReadMessage()

		if readErr != nil {
			break
		}

		if messageType != websocket.TextMessage {
			continue
		}

		var message ClientMessage

		err := json.Unmarshal(
			messageData,
			&message,
		)

		if err != nil {
			log.Println(
				"Invalid terminal message:",
				err,
			)
			continue
		}

		// ==============================================
		// TERMINAL INPUT
		// ==============================================

		if message.Type == "input" {
			_, err := stdin.Write(
				[]byte(message.Data),
			)

			if err != nil {
				log.Println(
					"stdin write error:",
					err,
				)
				break
			}
		}

		// ==============================================
		// TERMINAL RESIZE
		// ==============================================

		if message.Type == "resize" {
			if message.Cols <= 0 ||
				message.Rows <= 0 {
				continue
			}

			err := session.WindowChange(
				message.Rows,
				message.Cols,
			)

			if err != nil {
				log.Println(
					"PTY resize error:",
					err,
				)
			}
		}
	}

	// ==================================================
	// CLEANUP
	// ==================================================

	_ = stdin.Close()
	_ = session.Close()
	_ = outputWriter.Close()

	<-outputDone
}