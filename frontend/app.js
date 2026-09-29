const terminalButton =
    document.getElementById("terminalButton");

const guiButton =
    document.getElementById("guiButton");

const terminalPanel =
    document.getElementById("terminalPanel");

const guiPanel =
    document.getElementById("guiPanel");

const terminalElement =
    document.getElementById("terminal");

const guiFrame =
    document.getElementById("guiFrame");

const terminalStatus =
    document.getElementById("terminalStatus");


// ======================================================
// TERMINAL
// ======================================================

const terminal = new Terminal({
    cursorBlink: true,
    fontSize: 14,
    fontFamily: "monospace",
    convertEol: true,
    scrollback: 5000,

    theme: {
        background: "#000000",
        foreground: "#ffffff"
    }
});


const fitAddon =
    new FitAddon.FitAddon();

terminal.loadAddon(fitAddon);

terminal.open(terminalElement);

fitAddon.fit();


// ======================================================
// WEBSOCKET
// ======================================================

let socket = null;


async function requestTerminalTicket() {
    const accessToken = window.cyberlabAccessToken;
    const sessionID = window.cyberlabSessionId;

    if (!accessToken || !sessionID) {
        throw new Error("Authentication and a READY lab session are required.");
    }

    const response = await fetch(
        "/api/labs/sessions/" +
        encodeURIComponent(sessionID) +
        "/terminal-ticket",
        {
            method: "POST",
            headers: {
                "Authorization": "Bearer " + accessToken
            }
        }
    );

    if (!response.ok) {
        throw new Error("Unable to authorize the terminal.");
    }

    const payload = await response.json();
    if (!payload.ticket) {
        throw new Error("Unable to authorize the terminal.");
    }
    return payload.ticket;
}


async function connectTerminal() {

    if (
        socket &&
        socket.readyState === WebSocket.OPEN
    ) {
        return;
    }

    const protocol =
        window.location.protocol === "https:"
            ? "wss:"
            : "ws:";


    terminalStatus.textContent =
        "Connecting";


    terminal.write(
        "\r\nConnecting to Kali...\r\n"
    );


    let ticket;
    try {
        ticket = await requestTerminalTicket();
    } catch (error) {
        terminalStatus.textContent = "Unauthorized";
        terminal.write("\r\nTerminal authorization failed.\r\n");
        return;
    }

    const wsURL =
        protocol +
        "//" +
        window.location.host +
        "/api/terminal?ticket=" +
        encodeURIComponent(ticket);

    socket = new WebSocket(wsURL);


    socket.binaryType =
        "arraybuffer";


    // ==================================================
    // CONNECTED
    // ==================================================

    socket.onopen = () => {

        terminalStatus.textContent =
            "Connected";


        terminal.write(
            "\r\nConnected.\r\n\r\n"
        );


        sendResize();

        terminal.focus();
    };


    // ==================================================
    // SERVER → TERMINAL
    // ==================================================

    socket.onmessage =
        async (event) => {

            if (
                event.data instanceof ArrayBuffer
            ) {

                terminal.write(
                    new Uint8Array(
                        event.data
                    )
                );

            }

            else if (
                event.data instanceof Blob
            ) {

                const buffer =
                    await event.data.arrayBuffer();

                terminal.write(
                    new Uint8Array(buffer)
                );

            }

            else {

                terminal.write(
                    event.data
                );
            }
        };


    // ==================================================
    // DISCONNECTED
    // ==================================================

    socket.onclose = () => {

        terminalStatus.textContent =
            "Disconnected";


        terminal.write(
            "\r\n\r\nConnection closed.\r\n"
        );


        socket = null;
    };


    // ==================================================
    // ERROR
    // ==================================================

    socket.onerror = () => {

        terminalStatus.textContent =
            "Error";


        terminal.write(
            "\r\nTerminal connection error.\r\n"
        );
    };
}


// ======================================================
// TERMINAL INPUT
// ======================================================

terminal.onData((data) => {

    if (
        socket &&
        socket.readyState === WebSocket.OPEN
    ) {

        socket.send(
            JSON.stringify({
                type: "input",
                data: data
            })
        );
    }
});


// ======================================================
// TERMINAL RESIZE
// ======================================================

function sendResize() {

    if (
        !socket ||
        socket.readyState !== WebSocket.OPEN
    ) {
        return;
    }


    socket.send(
        JSON.stringify({
            type: "resize",
            cols: terminal.cols,
            rows: terminal.rows
        })
    );
}


window.addEventListener(
    "resize",
    () => {

        fitAddon.fit();

        sendResize();
    }
);


// ======================================================
// TERMINAL BUTTON
// ======================================================

terminalButton.addEventListener(
    "click",
    () => {

        terminalPanel.style.display =
            "block";

        guiPanel.style.display =
            "none";


        connectTerminal();


        setTimeout(() => {

            fitAddon.fit();

            terminal.focus();

            sendResize();

        }, 100);
    }
);


// ======================================================
// GUI BUTTON
// ======================================================
guiButton.addEventListener(
    "click",
    () => {
        window.location.href =
            "http://192.168.29.122:6080/vnc_auto.html";
    }
);


// ======================================================
// DEFAULT VIEW
// ======================================================

terminalPanel.style.display =
    "block";

guiPanel.style.display =
    "none";


connectTerminal();