// test/stream_test_helper.js
const CDP = require('chrome-remote-interface');
const fetch = require('node-fetch');
const path = require('path');
const fs = require('fs');

// Stream test configurations
const RTMP_STREAM_URL = 'rtmp://localhost/live/test';
const FLV_STREAM_URL = 'http://localhost:8080/flv/live/test';
const HLS_STREAM_URL = 'http://localhost:8080/hls/live/test/index.m3u8';

// Helper to simulate media playback using CDP
const testStreamPlayback = async (url, timeout = 10000) => {
  // Connect to CDP
  const client = await CDP();
  const { Page, Runtime, Network } = client;

  // Enable domains
  await Promise.all([
    Page.enable(),
    Runtime.enable(),
    Network.enable()
  ]);

  try {
    // Create HTML for testing video playback
    const testHtml = `
      <html>
        <head>
          <title>Stream Test</title>
          <style>
            video { width: 640px; height: 360px; }
          </style>
        </head>
        <body>
          <h1>Stream Test</h1>
          <div id="status">Loading...</div>
          <video id="player" controls autoplay></video>
          
          <script>
            const player = document.getElementById('player');
            const status = document.getElementById('status');
            let playing = false;
            let loaded = false;
            let error = false;
            
            // Set the source based on URL type
            if ('${url}'.endsWith('.m3u8')) {
              // HLS - use hls.js
              if (Hls.isSupported()) {
                const hls = new Hls();
                hls.loadSource('${url}');
                hls.attachMedia(player);
                hls.on(Hls.Events.MANIFEST_PARSED, function() {
                  player.play();
                });
                hls.on(Hls.Events.ERROR, function(event, data) {
                  error = true;
                  status.textContent = 'Error: ' + data.type + ' - ' + data.details;
                });
              }
            } else if ('${url}'.startsWith('rtmp://')) {
              // RTMP - use flash or other compatible element
              status.textContent = 'RTMP testing is done via server API check';
            } else {
              // FLV or regular video
              player.src = '${url}';
            }
            
            player.addEventListener('playing', () => {
              playing = true;
              status.textContent = 'Playing';
            });
            
            player.addEventListener('loadeddata', () => {
              loaded = true;
              status.textContent = 'Loaded';
            });
            
            player.addEventListener('error', (e) => {
              error = true;
              status.textContent = 'Error: ' + player.error.code;
            });
            
            // Expose variables for testing
            window.streamTest = {
              isPlaying: () => playing,
              isLoaded: () => loaded,
              hasError: () => error,
              getStatus: () => status.textContent
            };
          </script>
        </body>
      </html>
    `;

    // Navigate to a blank page and inject our HTML
    await Page.navigate({ url: 'about:blank' });
    await Page.loadEventFired();

    await Runtime.evaluate({
      expression: `document.write(\`${testHtml}\`); document.close();`
    });

    // Wait for video to start playing or timeout
    const startTime = Date.now();
    let isPlaying = false;
    let hasError = false;

    while (Date.now() - startTime < timeout && !isPlaying && !hasError) {
      // Check status
      const result = await Runtime.evaluate({
        expression: 'window.streamTest && window.streamTest.isPlaying ? window.streamTest.isPlaying() : false'
      });
      isPlaying = result.result.value;

      // Check for errors
      const errorResult = await Runtime.evaluate({
        expression: 'window.streamTest && window.streamTest.hasError ? window.streamTest.hasError() : false'
      });
      hasError = errorResult.result.value;

      if (!isPlaying && !hasError) {
        await new Promise(resolve => setTimeout(resolve, 500));
      }
    }

    // Get status
    const statusResult = await Runtime.evaluate({
      expression: 'window.streamTest && window.streamTest.getStatus ? window.streamTest.getStatus() : "Unknown"'
    });
    const status = statusResult.result.value;

    return {
      isPlaying,
      hasError,
      status,
      timeout: Date.now() - startTime >= timeout
    };
  } finally {
    // Close the CDP client
    await client.close();
  }
};

// Helper to check stream existence via API
const checkStreamExists = async (server, streamPath) => {
  try {
    const response = await fetch(`http://localhost:${server}/api/streams`);
    const streams = await response.json();

    // Check if stream exists in the list
    return streams.some(stream => stream.streamPath === streamPath);
  } catch (err) {
    console.error('Error checking stream:', err);
    return false;
  }
};

// Helper to test load balancing when multiple streaming consumers connect
const testLoadBalancedStreaming = async (cdp, numClients = 5) => {
  // First, ensure a test stream is available
  const streamExists = await checkStreamExists(8090, 'test/lb/stream1');

  if (!streamExists) {
    // Create a test stream if it doesn't exist
    const streamData = {
      streamPath: 'test/lb/stream1',
      publisherNodeId: 'node1',
    };

    await fetch('http://localhost:8090/cluster/api/streams/register', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(streamData)
    });
  }

  // Track client node assignments
  const assignments = [];

  // Simulate multiple clients connecting
  for (let i = 0; i < numClients; i++) {
    // Get load balancer suggestion
    const response = await fetch(`http://localhost:8090/cluster/api/loadbalancer/test?streamPath=test/lb/stream1&clientId=client${i}`);
    const decision = await response.json();

    assignments.push({
      clientId: `client${i}`,
      assignedNode: decision.selectedNode
    });
  }

  return assignments;
};

module.exports = {
  testStreamPlayback,
  checkStreamExists,
  testLoadBalancedStreaming,
  RTMP_STREAM_URL,
  FLV_STREAM_URL,
  HLS_STREAM_URL
}; 