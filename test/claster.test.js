// test/cluster.test.js
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
`;

// Setup and teardown
beforeAll(async () => {
  // Create configuration files
  const configPath1 = createConfig('claster_node1', clasterConfig1);
  const configPath2 = createConfig('claster_node2', clasterConfig2);

  // Start servers (but don't wait for them, as they're dependencies of each other)
  server1 = startClasterServer(configPath1);

  // Wait a bit before starting the second server
  await new Promise(resolve => setTimeout(resolve, 2000));

  server2 = startClasterServer(configPath2);

  // Connect to Chrome with CDP (assuming Chrome is already running with remote debugging port)
  cdp = await connectToCDP();
  client = cdp.client;

  // Wait for servers to be ready
  await waitForServerReady('http://localhost:8090/cluster/api/status', 15000);
  await waitForServerReady('http://localhost:8091/cluster/api/status', 15000);

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

  // Remove test config files if needed
  // fs.unlinkSync(path.join(CONFIG_DIR, 'claster_node1.yaml'));
  // fs.unlinkSync(path.join(CONFIG_DIR, 'claster_node2.yaml'));
});

// Test cases
describe('Cluster Plugin Core Functionality', () => {
  test('Should connect two nodes in a cluster', async () => {
    // Navigate to the Cluster status page
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8090/cluster/api/status' });
    await Page.loadEventFired();

    // Get cluster status via CDP
    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const statusText = result.result.value;
    const status = JSON.parse(statusText);

    // Verify that the cluster contains both nodes
    expect(status.nodes).toBeDefined();
    expect(Object.keys(status.nodes).length).toBe(2);
    expect(status.nodes).toHaveProperty('node1');
    expect(status.nodes).toHaveProperty('node2');
  });

  test('Stream registry synchronization between nodes', async () => {
    // Create a test stream on node1
    const streamData = {
      streamPath: 'test/stream1',
      publisherNodeId: 'node1',
    };

    // Register the stream
    const registerResponse = await fetch('http://localhost:8090/cluster/api/streams/register', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(streamData)
    });

    expect(registerResponse.status).toBe(200);

    // Wait for sync to happen
    await new Promise(resolve => setTimeout(resolve, 2000));

    // Check if the stream is visible on node2
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8091/cluster/api/streams' });
    await Page.loadEventFired();

    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const streamsText = result.result.value;
    const streams = JSON.parse(streamsText);

    // Verify stream synchronization
    expect(streams).toContainEqual(
      expect.objectContaining({
        streamPath: 'test/stream1',
        publisherNodeId: 'node1'
      })
    );
  });

  test('Load balancer directs client to appropriate node', async () => {
    // Navigate to the load balancer test page
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8090/cluster/api/loadbalancer/test?streamPath=test/stream1' });
    await Page.loadEventFired();

    // Get load balancer decision
    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const lbText = result.result.value;
    const lbDecision = JSON.parse(lbText);

    // Verify that the load balancer returns a valid node
    expect(lbDecision).toHaveProperty('selectedNode');
    expect(lbDecision.selectedNode).toBe('node1'); // Should be node1 since it's the publisher
  });
});

describe('Cluster Plugin UI Tests', () => {
  test('Dashboard displays all nodes correctly', async () => {
    // Navigate to the dashboard
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8090/ui/dashboard' });
    await Page.loadEventFired();

    // Wait for dashboard to load
    await new Promise(resolve => setTimeout(resolve, 2000));

    // Check for node elements
    const nodesResult = await Runtime.evaluate({
      expression: 'document.querySelectorAll(".node-card").length'
    });

    expect(nodesResult.result.value).toBe(2);

    // Check node details
    const node1NameResult = await Runtime.evaluate({
      expression: 'Array.from(document.querySelectorAll(".node-card")).find(el => el.textContent.includes("node1")).querySelector(".node-name").textContent'
    });

    expect(node1NameResult.result.value).toContain('node1');
  });

  test('Stream list displays active streams', async () => {
    // Navigate to the streams page
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8090/ui/streams' });
    await Page.loadEventFired();

    // Wait for streams to load
    await new Promise(resolve => setTimeout(resolve, 2000));

    // Check for our test stream
    const streamResult = await Runtime.evaluate({
      expression: 'document.querySelector(".stream-list").textContent'
    });

    expect(streamResult.result.value).toContain('test/stream1');
  });
});

describe('Cluster Plugin Health Monitoring', () => {
  test('Health status should report all nodes healthy', async () => {
    // Navigate to health status page
    const { Page, Runtime } = cdp;
    await Page.navigate({ url: 'http://localhost:8090/cluster/api/health' });
    await Page.loadEventFired();

    // Get health status
    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const healthText = result.result.value;
    const healthStatus = JSON.parse(healthText);

    // Verify health status
    expect(healthStatus).toHaveProperty('nodes');
    expect(healthStatus.nodes).toHaveProperty('node1');
    expect(healthStatus.nodes).toHaveProperty('node2');
    expect(healthStatus.nodes.node1.status).toBe('healthy');
    expect(healthStatus.nodes.node2.status).toBe('healthy');
  });
});

// Performance tests
describe('Cluster Plugin Performance Tests', () => {
  test('Should handle multiple stream registrations quickly', async () => {
    const startTime = Date.now();
    const numStreams = 10;

    // Register multiple streams in parallel
    const promises = [];
    for (let i = 0; i < numStreams; i++) {
      const streamData = {
        streamPath: `test/perf/stream${i}`,
        publisherNodeId: i % 2 === 0 ? 'node1' : 'node2',
      };

      promises.push(
        fetch('http://localhost:8090/cluster/api/streams/register', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(streamData)
        })
      );
    }

    await Promise.all(promises);
    const endTime = Date.now();
    const duration = endTime - startTime;

    // Check performance (registration should take less than 500ms)
    expect(duration).toBeLessThan(500);

    // Verify all streams were registered
    const response = await fetch('http://localhost:8090/cluster/api/streams');
    const streams = await response.json();

    // Count our test streams
    const testStreams = streams.filter(s => s.streamPath.startsWith('test/perf/'));
    expect(testStreams.length).toBe(numStreams);
  });
}); 