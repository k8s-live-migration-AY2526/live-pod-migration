#!/usr/bin/env python3
import time
import socket
import logging
from flask import Flask, jsonify, request, Response, session

# Configure logging
logging.basicConfig(
    level=logging.INFO, format="%(asctime)s - %(name)s - %(levelname)s - %(message)s"
)
logger = logging.getLogger(__name__)

app = Flask(__name__)
app.secret_key = "migration-test"  # for session cookies

# Globals
counter = 0
logger.info("Init global counter: %s", counter)
clients = {}


@app.route("/slow_sleep_sync", methods=["GET"])
def slow_sleep_sync():
    time.sleep(60)
    return "Done\n"


@app.route("/slow_work", methods=["GET"])
def slow_work():
    start = time.time()
    i = 0
    while time.time() - start < 60:
        i += 1
    return f"{str(i)}\n"


@app.route("/health", methods=["GET"])
def health_check():
    logger.info("Health check endpoint called")
    return jsonify({"status": "healthy", "message": "Server is running"}), 200


@app.route("/counter", methods=["POST", "GET"])
def counter_endpoint():
    global counter
    counter += 1
    logger.info(f"Counter endpoint called - current count: {counter}")
    return jsonify({"count": counter}), 200


@app.route("/client", methods=["GET"])
def client_info():
    ip = request.remote_addr
    ua = request.headers.get("User-Agent", "unknown")
    clients[ip] = clients.get(ip, 0) + 1
    return jsonify({"client_ip": ip, "user_agent": ua, "times_seen": clients[ip]}), 200


@app.route("/stream", methods=["GET"])
def stream():
    def generate():
        for i in range(30):
            yield f"chunk {i}\n"
            time.sleep(2)

    return Response(generate(), mimetype="text/plain")


@app.route("/session", methods=["GET"])
def session_counter():
    session["count"] = session.get("count", 0) + 1
    return jsonify({"session_count": session["count"]}), 200


import socket

def get_pod_ip():
    try:
        # open a dummy socket connection to a well-known address
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))   # doesn't actually send packets
        ip = s.getsockname()[0]
        s.close()
        return ip
    except Exception:
        return "unresolved"


@app.route("/whoami", methods=["GET"])
def whoami():
    hostname = socket.gethostname()
    ip = get_pod_ip()
    return jsonify({"hostname": hostname, "ip": ip})


@app.route("/", methods=["GET"])
def root():
    return (
        jsonify(
            {
                "message": "Kubernetes migration test server",
                "endpoints": {
                    "/health": "Health check",
                    "/counter": "Global counter",
                    "/client": "Tracks clients by IP and User-Agent",
                    "/session": "Session cookie counter",
                    "/stream": "Streaming response",
                    "/ws": "WebSocket echo (requires flask-sock)",
                    "/whoami": "Shows pod hostname and IP",
                    "/slow_sleep_sync": "Long sleep endpoint",
                    "/slow_work": "Busy loop endpoint",
                },
            }
        ),
        200,
    )


from flask_sock import Sock

sock = Sock(app)


@sock.route("/ws")
def ws(sock):
    while True:
        data = sock.receive()
        if data is None:
            break
        sock.send(f"echo: {data}")


if __name__ == "__main__":
    logger.info("Starting server on port 8080")
    app.run(host="0.0.0.0", port=8080, debug=False)
