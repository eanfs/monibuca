// test/setup.js
const CDP = require('chrome-remote-interface');
const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');

// Configuration variables
const CONFIG_DIR = path.join(__dirname, 'configs');
const TIMEOUT = 10000;

// Ensure the config directory exists
if (!fs.existsSync(CONFIG_DIR)) {
  fs.mkdirSync(CONFIG_DIR, { recursive: true });
}

// Helper to create a configuration file for testing
const createConfig = (name, config) => {
  const configPath = path.join(CONFIG_DIR, `${name}.yaml`);
  fs.writeFileSync(configPath, config);
  return configPath;
};

// Connect to Chrome using CDP
const connectToCDP = async (port = 9222) => {
  try {
    const client = await CDP({ port });
    const { Network, Page, Runtime } = client;

    // Enable necessary domains
    await Promise.all([
      Network.enable(),
      Page.enable(),
    ]);

    return { client, Network, Page, Runtime };
  } catch (err) {
    console.error('Cannot connect to Chrome:', err);
    throw err;
  }
};

// Start Cluster server with configuration
const startClasterServer = (configPath, options = {}) => {
  const serverProcess = spawn('go', ['run', '-tags', 'sqlite', 'main.go', '-c', configPath], {
    cwd: path.join(__dirname, '../example/default'),
    ...options
  });

  return serverProcess;
};

// Wait for server to be ready
const waitForServerReady = async (url, timeout = TIMEOUT) => {
  const startTime = Date.now();

  while (Date.now() - startTime < timeout) {
    try {
      const response = await fetch(url);
      if (response.ok) {
        return true;
      }
    } catch (err) {
      // Server not ready yet, wait and retry
      await new Promise(resolve => setTimeout(resolve, 500));
    }
  }

  throw new Error(`Server failed to start within ${timeout}ms`);
};

module.exports = {
  createConfig,
  connectToCDP,
  startClasterServer,
  waitForServerReady,
  CONFIG_DIR,
  TIMEOUT
}; 