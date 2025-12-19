#!/usr/bin/env node

/**
 * Cluster 插件测试运行器
 * 此脚本用于演示如何使用 CDP 和 Jest 测试 Cluster 插件
 */

const path = require('path');
const { spawn } = require('child_process');
const CDP = require('chrome-remote-interface');

// 测试配置
const TEST_PORT = 9222;
const CONFIG_DIR = path.join(__dirname, '.');
const CONFIG_FILES = {
  node1: path.join(CONFIG_DIR, 'node1.yaml'),
  node2: path.join(CONFIG_DIR, 'node2.yaml'),
  node3: path.join(CONFIG_DIR, 'node3.yaml')
};

// 启动服务器进程
const servers = {};

// 一个简单的延迟函数
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));

// 启动 Cluster 服务器
async function startServers() {
  console.log('启动 Cluster 服务器...');

  // 首先启动管理节点
  servers.node1 = spawn('go', ['run', '-tags', 'sqlite', 'main.go', '-c', CONFIG_FILES.node1], {
    cwd: path.join(__dirname, '../../example/default'),
    stdio: 'inherit'
  });

  // 等待管理节点启动
  await delay(5000);

  // 启动工作节点
  servers.node2 = spawn('go', ['run', '-tags', 'sqlite', 'main.go', '-c', CONFIG_FILES.node2], {
    cwd: path.join(__dirname, '../../example/default'),
    stdio: 'inherit'
  });

  servers.node3 = spawn('go', ['run', '-tags', 'sqlite', 'main.go', '-c', CONFIG_FILES.node3], {
    cwd: path.join(__dirname, '../../example/default'),
    stdio: 'inherit'
  });

  // 等待所有节点启动
  await delay(5000);
  console.log('所有 Cluster 服务器已启动');
}

// 连接到 CDP
async function connectToCDP() {
  try {
    console.log(`连接到 Chrome DevTools Protocol (端口 ${TEST_PORT})...`);
    const client = await CDP({ port: TEST_PORT });
    const { Network, Page, Runtime } = client;

    await Promise.all([
      Network.enable(),
      Page.enable()
    ]);

    console.log('CDP 连接成功');
    return { client, Network, Page, Runtime };
  } catch (err) {
    console.error('无法连接到 Chrome:', err);
    throw err;
  }
}

// 运行一个测试用例
async function runTest() {
  let client;

  try {
    const cdp = await connectToCDP();
    client = cdp.client;
    const { Page, Runtime } = cdp;

    console.log('运行集群状态测试...');

    // 导航到集群状态页面
    await Page.navigate({ url: 'http://localhost:8090/cluster/api/status' });
    await Page.loadEventFired();

    // 获取集群状态
    const result = await Runtime.evaluate({
      expression: 'document.body.textContent'
    });

    const statusText = result.result.value;
    const status = JSON.parse(statusText);

    // 验证集群状态
    console.log('集群状态:', JSON.stringify(status, null, 2));

    const nodeCount = Object.keys(status.nodes).length;
    if (nodeCount === 3) {
      console.log('✅ 测试通过: 集群包含所有预期的节点');
    } else {
      console.error(`❌ 测试失败: 集群应该包含 3 个节点，但实际有 ${nodeCount} 个`);
    }

    if (status.nodes.node1 && status.nodes.node2 && status.nodes.node3) {
      console.log('✅ 测试通过: 找到所有预期的节点 ID');
    } else {
      console.error('❌ 测试失败: 缺少一个或多个预期的节点');
    }

  } catch (err) {
    console.error('测试期间出错:', err);
  } finally {
    if (client) {
      await client.close();
      console.log('CDP 客户端已关闭');
    }
  }
}

// 清理函数
function cleanup() {
  console.log('清理资源...');

  // 终止所有服务器进程
  Object.values(servers).forEach(server => {
    if (server) {
      server.kill();
    }
  });

  console.log('所有服务器已停止');
}

// 主函数
async function main() {
  try {
    // 尝试启动服务器
    await startServers();

    // 运行测试
    await runTest();

  } catch (err) {
    console.error('测试过程中出错:', err);
  } finally {
    // 清理资源
    cleanup();
  }
}

// 处理进程终止信号
process.on('SIGINT', () => {
  console.log('\n接收到 SIGINT 信号，正在清理...');
  cleanup();
  process.exit(0);
});

// 运行测试
main().catch(err => {
  console.error('未处理的错误:', err);
  cleanup();
  process.exit(1);
}); 