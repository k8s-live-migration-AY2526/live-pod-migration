#!/usr/bin/env python3
"""
Minimal Python HTTP server with health check and counter endpoints.
"""

import logging
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
            "/counter": "Counter endpoint (increments on each call)"
        }
    }), 200

if __name__ == '__main__':
    logger.info("Starting server on port 8080")
    app.run(host='0.0.0.0', port=8080, debug=False)
