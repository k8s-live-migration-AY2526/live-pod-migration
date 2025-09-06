#!/usr/bin/env python3
"""
Minimal Python HTTP server with health check and counter endpoints.
"""

import time
import logging
import asyncio
from flask import Flask, jsonify

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Global counter variable
counter = 0
logger.info("Init global counter: %s", counter)

@app.route('/slow_sleep_async', methods=['GET'])
async def slow_sleep_async():
    await asyncio.sleep(15)
    return "Done\n"

@app.route('/slow_sleep_sync', methods=['GET'])
def slow_sleep_sync():
    time.sleep(15)
    return "Done\n"

@app.route('/slow_work', methods=['GET'])
def slow_work():
    start = time.time()
    i = 0
    while time.time() - start < 15:
        i += 1
    return f"{str(i)}\n"

@app.route('/health', methods=['GET'])
def health_check():
    """Health check endpoint"""
    logger.info("Health check endpoint called")
    return jsonify({
        "status": "healthy",
        "message": "Server is running"
    }), 200

@app.route('/counter', methods=['POST', 'GET'])
def counter_endpoint():
    """Counter endpoint - increments on each call"""
    global counter
    counter += 1
    logger.info(f"Counter endpoint called - current count: {counter}")
    return jsonify({
        "count": counter,
        "message": f"Counter incremented to {counter}"
    }), 200

@app.route('/', methods=['GET'])
def root():
    """Root endpoint for basic info"""
    logger.info("Root endpoint called")
    return jsonify({
        "message": "Minimal Python HTTP Server",
        "endpoints": {
            "/health": "Health check endpoint",
            "/counter": "Counter endpoint (increments on each call)",
            "/slow_sleep_async": "Long running endpoint, asyncio sleep",
            "/slow_sleep_sync": "Long running endpoint, time sleep",
            "/slow_work": "Long running endpoint, busy loop",
        }
    }), 200

if __name__ == '__main__':
    logger.info("Starting server on port 8080")
    app.run(host='0.0.0.0', port=8080, debug=False)
