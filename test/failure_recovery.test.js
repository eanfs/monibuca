const {
  createConfig,
  connectToCDP,
  startClasterServer,
  waitForServerReady,
  CONFIG_DIR,
  TIMEOUT
} = require('./setup');
const fetch = require('node-fetch');
const path = require('path');
const fs = require('fs');

// Global variables
let server1;
let server2;
let server3;
let client;
let cdp;

// Create test configurations
const clasterConfig1 = `
cluster:
  localNodeId: "node1"
  localNodeRole: "manager"
  listenAddr: ":8090"
  advertiseAddr: "127.0.0.1:8090"
  seedNodes: []
  enableMetrics: true
  metricInterval: 5
  healthCheckInterval: 2
  failureDetectionThreshold: 3
`;

const clasterConfig2 = `
cluster:
  localNodeId: "node2"
  localNodeRole: "worker"
  listenAddr: ":8091"
  advertiseAddr: "127.0.0.1:8091"
  seedNodes: ["127.0.0.1:8090"]
  enableMetrics: true
  metricInterval: 5
  healthCheckInterval: 2
  failureDetectionThreshold: 3
`;

const clasterConfig3 = `
cluster:
  localNodeId: "node3"
  localNodeRole: "worker"
  listenAddr: ":8092"
  advertiseAddr: "127.0.0.1:8092"
  seedNodes: ["127.0.0.1:8090"]
  enableMetrics: true
  metricInterval: 5
  healthCheckInterval: 2
  failureDetectionThreshold: 3
`;

// Setup and teardown
beforeAll(async () => {
  // Create configuration files
  const configPath1 = createConfig('claster_node1_failure', clasterConfig1);
  const configPath2 = createConfig('claster_node2_failure', clasterConfig2);
  const configPath3 = createConfig('claster_node3_failure', clasterConfig3);

  // Start servers
  server1 = startClasterServer(configPath1);

  // Wait a bit before starting the other servers
  await new Promise(resolve => setTimeout(resolve, 2000));

  server2 = startClasterServer(configPath2);
  server3 = startClasterServer(configPath3);

  // Connect to Chrome with CDP (assuming Chrome is already running with remote debugging port)
  cdp = await connectToCDP();
  client = cdp.client;

  // Wait for servers to be ready
  await waitForServerReady('http://localhost:8090/cluster/api/status', 15000);
  await waitForServerReady('http://localhost:8091/cluster/api/status', 15000);
  await waitForServerReady('http://localhost:8092/cluster/api/status', 15000);

  // Add additional setup if needed
}, 30000); // Longer timeout for setup

afterAll(async () => {
  // Clean up resources
  if (client) {
    await client.close();
  }

  if (server1) {
    server1.kill();
  }

  if (server2) {
    server2.kill();
  }

  if (server3) {
    server3.kill();
  }
});

// Test cases
describe('Cluster Plugin Failure and Recovery', () => {
  test('Should detect node failure and update cluster state', async () => {
    // First, verify all nodes are present
    let response = await fetch('http://localhost:8090/cluster/api/status');
    let status = await response.json();

    expect(Object.keys(status.nodes).length).toBe(3);
    expect(status.nodes).toHaveProperty('node1');
    expect(status.nodes).toHaveProperty('node2');
    expect(status.nodes).toHaveProperty('node3');

    // Kill node2 to simulate failure
    if (server2) {
      server2.kill();
      server2 = null;
    }

    // Wait for failure detection (slightly longer than the failure detection threshold)
    await new Promise(resolve => setTimeout(resolve, 8000));

    // Check if node2 is marked as failed
    response = await fetch('http://localhost:8090/cluster/api/status');
    status = await response.json();

    expect(status.nodes.node2.status).toBe('failed');

    // Check via CDP
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8090/cluster/api/health' });
    await Page.loadEventFired();

    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const healthText = result.result.value;
    const healthStatus = JSON.parse(healthText);

    expect(healthStatus.nodes.node2.status).toBe('failed');
  }, 20000);

  test('Failover should reassign streams to healthy nodes', async () => {
    // Create a test stream assigned to node2 (which is now failed)
    const streamData = {
      streamPath: 'test/failure/stream1',
      publisherNodeId: 'node2',
    };

    await fetch('http://localhost:8090/cluster/api/streams/register', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(streamData)
    });

    // Wait for failover to happen
    await new Promise(resolve => setTimeout(resolve, 5000));

    // Check if the stream was reassigned
    const response = await fetch('http://localhost:8090/cluster/api/streams');
    const streams = await response.json();

    // Find our test stream
    const testStream = streams.find(s => s.streamPath === 'test/failure/stream1');

    expect(testStream).toBeDefined();
    expect(testStream.publisherNodeId).not.toBe('node2'); // Should be reassigned
    expect(['node1', 'node3']).toContain(testStream.publisherNodeId); // Should be assigned to a healthy node
  }, 10000);

  test('Should handle node recovery', async () => {
    // Restart node2
    const configPath2 = path.join(CONFIG_DIR, 'claster_node2_failure.yaml');
    server2 = startClasterServer(configPath2);

    // Wait for node to reconnect and be detected
    await new Promise(resolve => setTimeout(resolve, 10000));

    // Check if node2 is back
    const response = await fetch('http://localhost:8090/cluster/api/status');
    const status = await response.json();

    expect(status.nodes.node2.status).toBe('healthy');

    // Check via CDP
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8090/cluster/api/health' });
    await Page.loadEventFired();

    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const healthText = result.result.value;
    const healthStatus = JSON.parse(healthText);

    expect(healthStatus.nodes.node2.status).toBe('healthy');
  }, 20000);

  test('Should redistribute load after recovery', async () => {
    // Register multiple streams
    const numStreams = 5;

    for (let i = 0; i < numStreams; i++) {
      const streamData = {
        streamPath: `test/recovery/stream${i}`,
        publisherNodeId: i % 2 === 0 ? 'node1' : 'node3', // Distribute between node1 and node3
      };

      await fetch('http://localhost:8090/cluster/api/streams/register', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(streamData)
      });
    }

    // Wait for potential redistribution
    await new Promise(resolve => setTimeout(resolve, 10000));

    // Get current stream distribution
    const response = await fetch('http://localhost:8090/cluster/api/streams');
    const streams = await response.json();

    // Count streams per node
    const streamCounts = {
      node1: 0,
      node2: 0,
      node3: 0
    };

    // Only count our test streams
    streams
      .filter(s => s.streamPath.startsWith('test/recovery/'))
      .forEach(s => {
        streamCounts[s.publisherNodeId]++;
      });

    // Verify node2 has been allocated some streams after recovery
    expect(streamCounts.node2).toBeGreaterThan(0);
  }, 15000);

  test('Manager node failure should trigger leader election', async () => {
    // Kill node1 (the manager)
    if (server1) {
      server1.kill();
      server1 = null;
    }

    // Wait for leader election
    await new Promise(resolve => setTimeout(resolve, 10000));

    // Check who is the new leader (should be node3 since node2 just recovered)
    // This requires accessing status from node3
    const response = await fetch('http://localhost:8092/cluster/api/status');
    const status = await response.json();

    // Find which node is now the leader
    let newLeader = null;
    Object.keys(status.nodes).forEach(nodeId => {
      if (status.nodes[nodeId].role === 'manager') {
        newLeader = nodeId;
      }
    });

    expect(newLeader).not.toBe('node1');
    expect(['node2', 'node3']).toContain(newLeader);

    // Check via CDP
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8092/cluster/api/status' });
    await Page.loadEventFired();

    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const statusText = result.result.value;
    const statusData = JSON.parse(statusText);

    // Verify the leader is properly set
    let cdpLeader = null;
    Object.keys(statusData.nodes).forEach(nodeId => {
      if (statusData.nodes[nodeId].role === 'manager') {
        cdpLeader = nodeId;
      }
    });

    expect(cdpLeader).toBe(newLeader);
  }, 15000);
}); 