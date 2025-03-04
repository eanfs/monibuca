#!/usr/bin/env node

/**
 * Cluster 插件 Etcd 单节点测试运行器
 * 此脚本用于测试单个 Cluster 节点的 Etcd 集成功能
 */

const path = require('path');
const { spawn } = require('child_process');
const CDP = require('chrome-remote-interface');
const fetch = require('node-fetch');
const { execSync } = require('child_process');

// 测试配置
const TEST_PORT = 9222;
const HTTP_PORT = 8080;
const CONFIG_DIR = path.join(__dirname, '.');
const CONFIG_FILE = path.join(CONFIG_DIR, 'etcd-node1.yaml');

// 启动服务器进程
let server = null;

// 一个简单的延迟函数
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));

// 检查 etcd 服务器是否就绪
async function checkEtcdServer() {
  console.log('等待 Etcd 服务器启动...');
  let retries = 10;
  while (retries > 0) {
    try {
      console.log(`尝试连接 Etcd 服务器 (http://localhost:2379/health)...`);
      const response = await fetch('http://localhost:2379/health');
      const status = await response.json();
      console.log('Etcd 服务器响应:', status);

      if (status.health === 'true') {
        console.log('✅ Etcd 服务器已就绪');
        return true;
      }
      console.log('Etcd 服务器未就绪，等待中...');
    } catch (err) {
      console.log(`等待 Etcd 服务器启动 (${retries} 次尝试剩余): ${err.message}`);
      // 检查端口是否被占用
      try {
        const portCheck = execSync('lsof -i :2379').toString();
        console.log('端口 2379 状态:', portCheck);
      } catch (e) {
        console.log('端口 2379 未被占用');
      }
    }
    await delay(2000);
    retries--;
  }
  console.error('❌ Etcd 服务器启动超时');
  return false;
}

// 启动 Cluster 服务器
async function startServer() {
  console.log('启动 Cluster 服务器 (Etcd 模式)...');

  // 设置环境变量
  const env = {
    ...process.env,
    GODEBUG: 'protobuf=2',  // 启用详细的 protobuf 调试信息
    GO_TESTMODE: '1',       // 启用测试模式
    ETCDCTL_API: '3'        // 使用 etcd v3 API
  };

  // 确保数据目录存在
  const dataDir = path.join(__dirname, 'data', 'etcd1');
  try {
    execSync(`mkdir -p ${dataDir}`);
    console.log(`✅ 创建数据目录: ${dataDir}`);
  } catch (err) {
    console.error(`❌ 创建数据目录失败: ${err.message}`);
  }

  // 启动节点
  server = spawn('go', ['run', '-tags', 'sqlite,dummy', 'etcd-main.go', '-c', CONFIG_FILE], {
    cwd: path.join(__dirname, '.'),
    stdio: ['ignore', 'pipe', 'pipe'],  // 只保留 stdout 和 stderr
    env
  });

  // 处理输出
  server.stdout.on('data', (data) => {
    const output = data.toString().trim();
    console.log(`[Node] ${output}`);

    // 检查关键日志
    if (output.includes('etcd server is ready')) {
      console.log('✅ Etcd 服务器已就绪');
    }
    if (output.includes('Node registered successfully')) {
      console.log('✅ 节点注册成功');
    }
    if (output.includes('Node sync completed')) {
      console.log('✅ 节点同步完成');
    }
    if (output.includes('failed to start etcd server')) {
      console.error('❌ Etcd 服务器启动失败');
    }
    if (output.includes('etcd server error')) {
      console.error('❌ Etcd 服务器错误');
    }
    if (output.includes('Starting embedded etcd server')) {
      console.log('🔄 正在启动内嵌 Etcd 服务器...');
    }
    if (output.includes('Creating etcd client')) {
      console.log('🔄 正在创建 Etcd 客户端...');
    }
    if (output.includes('Starting cluster manager')) {
      console.log('🔄 正在启动集群管理器...');
    }
    if (output.includes('Starting stream synchronization service')) {
      console.log('🔄 正在启动流同步服务...');
    }
    if (output.includes('Starting resource optimizer')) {
      console.log('🔄 正在启动资源优化器...');
    }
  });

  server.stderr.on('data', (data) => {
    const error = data.toString().trim();
    console.error(`[Node Error] ${error}`);

    // 检查错误日志
    if (error.includes('failed to create etcd client')) {
      console.error('❌ Etcd 客户端创建失败');
    }
    if (error.includes('failed to register node')) {
      console.error('❌ 节点注册失败');
    }
    if (error.includes('failed to sync nodes')) {
      console.error('❌ 节点同步失败');
    }
    if (error.includes('etcd server error')) {
      console.error('❌ Etcd 服务器错误');
    }
    if (error.includes('failed to start etcd')) {
      console.error('❌ Etcd 服务器启动失败');
    }
    if (error.includes('etcd took too long to start')) {
      console.error('❌ Etcd 服务器启动超时');
    }
    if (error.includes('failed to create data directory')) {
      console.error('❌ 创建数据目录失败');
    }
    if (error.includes('invalid listen client url')) {
      console.error('❌ 无效的客户端监听地址');
    }
    if (error.includes('invalid advertise client url')) {
      console.error('❌ 无效的客户端广播地址');
    }
  });

  // 等待节点和 etcd 启动
  console.log('等待节点和内嵌 etcd 启动...');
  await delay(5000); // 先等待 5 秒让进程启动

  // 检查 etcd 服务器是否就绪
  if (!await checkEtcdServer()) {
    // 如果服务器启动失败，尝试使用 etcdctl 检查状态
    try {
      console.log('尝试使用 etcdctl 检查状态...');
      const etcdctlStatus = execSync('etcdctl endpoint health').toString();
      console.log('etcdctl 状态:', etcdctlStatus);
    } catch (err) {
      console.error('etcdctl 检查失败:', err.message);
    }
    throw new Error('Etcd 服务器启动失败');
  }

  // 等待节点注册和同步
  console.log('等待节点注册和同步...');
  await delay(10000);
  console.log('Cluster 服务器已启动');
}

// 连接到 CDP
async function connectToCDP() {
  try {
    console.log(`连接到 Chrome DevTools Protocol (端口 ${TEST_PORT})...`);

    // 等待Chrome启动并开始接收连接
    let retries = 10;
    while (retries > 0) {
      try {
        // 尝试获取可用的调试目标
        const targets = await CDP.List({ port: TEST_PORT });
        if (targets && targets.length > 0) {
          console.log(`找到 ${targets.length} 个可调试目标`);
          break;
        }
        console.log('没有找到可调试目标，等待Chrome启动...');
      } catch (e) {
        console.log(`等待Chrome启动 (${retries} 次尝试剩余): ${e.message}`);
      }

      await delay(1000);
      retries--;

      if (retries === 0) {
        console.log('无法找到可调试目标，尝试打开一个新页面...');
        // 打开一个新标签页
        try {
          execSync(`open -a "Google Chrome" http://localhost:${HTTP_PORT}/`);
          await delay(2000);
        } catch (e) {
          console.error('打开新页面失败:', e);
        }
      }
    }

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

// 测试节点状态
async function testNodeStatus() {
  try {
    console.log('\n测试: 节点状态');

    // 首先检查 etcd 服务器状态
    console.log('检查 Etcd 服务器状态...');
    const etcdResponse = await fetch('http://localhost:2379/health');
    const etcdStatus = await etcdResponse.json();
    console.log('Etcd 服务器状态:', etcdStatus);

    if (etcdStatus.health !== 'true') {
      console.error('❌ Etcd 服务器不健康');
      return false;
    }
    console.log('✅ Etcd 服务器健康');

    // 检查集群状态
    const response = await fetch(`http://localhost:${HTTP_PORT}/cluster/api/cluster/status`);
    const text = await response.text();
    console.log('原始响应:', text);

    try {
      const status = JSON.parse(text);
      console.log('节点状态:', JSON.stringify(status, null, 2));

      // 检查基本状态字段
      if (status.status.totalNodes === 1) {
        console.log('✅ 测试通过: 节点数量正确');
      } else {
        console.error(`❌ 测试失败: 节点数量不正确，期望 1，实际 ${status.status.totalNodes}`);
        // 输出更多调试信息
        console.log('当前节点列表:', status.status.nodes);
      }

      // 检查健康节点数量
      if (status.status.healthyNodes === 1) {
        console.log('✅ 测试通过: 节点处于健康状态');
      } else {
        console.error(`❌ 测试失败: 健康节点数量不正确，期望 1，实际 ${status.status.healthyNodes}`);
      }

      // 检查集群状态
      if (status.status.clusterState === "normal") {
        console.log('✅ 测试通过: 集群状态正常');
      } else {
        console.error(`❌ 测试失败: 集群状态异常，期望 "normal"，实际 "${status.status.clusterState}"`);
      }

      // 获取节点列表进行详细检查
      const nodesResponse = await fetch(`http://localhost:${HTTP_PORT}/cluster/api/nodes`);
      const nodesData = await nodesResponse.json();
      console.log('节点列表数据:', JSON.stringify(nodesData, null, 2));

      if (nodesData.nodes && nodesData.nodes.length === 1) {
        console.log('✅ 测试通过: 节点列表正确');

        // 检查节点ID
        const node = nodesData.nodes[0];
        if (node.id === 'etcd-node1') {
          console.log('✅ 测试通过: 节点ID正确');
        } else {
          console.error(`❌ 测试失败: 节点ID不正确，期望 "etcd-node1"，实际 "${node.id}"`);
        }

        // 检查节点角色
        if (node.role === 'manager') {
          console.log('✅ 测试通过: 节点角色正确');
        } else {
          console.error(`❌ 测试失败: 节点角色不正确，期望 "manager"，实际 "${node.role}"`);
        }
      } else {
        console.error(`❌ 测试失败: 节点列表数量不正确，期望 1，实际 ${nodesData.nodes ? nodesData.nodes.length : 0}`);
      }

    } catch (parseErr) {
      console.error('解析响应失败:', parseErr);
      return false;
    }

    return true;
  } catch (err) {
    console.error('节点状态测试出错:', err);
    return false;
  }
}

// 测试 etcd 流注册
async function testStreamRegistration() {
  try {
    console.log('\n测试: Etcd 流注册功能');

    // 注册一个测试流
    const streamPath = 'test-stream-' + Date.now();
    const streamInfo = {
      streamPath: streamPath,
      publisherNodeID: 'etcd-node1',
      startTime: new Date().toISOString(),
      lastUpdated: new Date().toISOString(),
      bitrateMbps: 1.0
    };

    // 注册流
    const registerResponse = await fetch(`http://localhost:${HTTP_PORT}/cluster/api/streams`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(streamInfo)
    });

    const registerResult = await registerResponse.json();
    if (!registerResult.success) {
      console.error('❌ 测试失败: 无法注册流');
      return false;
    }

    console.log('✅ 测试通过: 成功注册流');

    // 等待流信息同步
    await delay(2000);

    // 获取流信息验证
    const getResponse = await fetch(`http://localhost:${HTTP_PORT}/cluster/api/streams/${streamPath}`);
    const getResult = await getResponse.json();

    if (getResult.success && getResult.streamInfo && getResult.streamInfo.streamPath === streamPath) {
      console.log('✅ 测试通过: 成功获取流信息');
    } else {
      console.error('❌ 测试失败: 无法获取流信息');
      return false;
    }

    // 清理测试数据
    const unregisterResponse = await fetch(`http://localhost:${HTTP_PORT}/cluster/api/streams/${streamPath}`, {
      method: 'DELETE'
    });

    const unregisterResult = await unregisterResponse.json();
    if (!unregisterResult.success) {
      console.error('❌ 测试失败: 无法清理测试数据');
      return false;
    }

    console.log('✅ 测试通过: 成功清理测试数据');
    return true;
  } catch (err) {
    console.error('流注册测试出错:', err);
    return false;
  }
}

// 清理函数
function cleanup() {
  console.log('清理资源...');

  // 终止服务器进程
  if (server && !server.killed) {
    try {
      // 发送 SIGTERM 信号
      server.kill('SIGTERM');

      // 如果进程还在运行，强制结束
      if (!server.killed) {
        server.kill('SIGKILL');
      }
    } catch (err) {
      console.error(`终止服务器进程失败:`, err);
    }
  }

  // 使用 pkill 确保所有相关进程都被终止
  try {
    execSync('pkill -f "etcd-main"');
  } catch (err) {
    // 忽略错误，因为可能没有进程需要终止
  }

  console.log('所有服务器已停止');
}

// 确保在程序退出时清理
process.on('exit', cleanup);
process.on('SIGTERM', () => {
  console.log('\n接收到 SIGTERM 信号，正在清理...');
  cleanup();
  process.exit(0);
});

// 处理进程终止信号
process.on('SIGINT', () => {
  console.log('\n接收到 SIGINT 信号，正在清理...');
  cleanup();
  process.exit(0);
});

// 主函数
async function main() {
  try {
    // 尝试启动服务器
    await startServer();

    // 连接到CDP（由用户预先打开的Chrome浏览器）
    const cdp = await connectToCDP();
    console.log('已成功连接到Chrome浏览器');

    // 运行各种测试
    const tests = [
      { name: "节点状态", fn: testNodeStatus },
      { name: "流注册功能", fn: testStreamRegistration }
    ];

    let passedCount = 0;
    let failedCount = 0;

    for (const test of tests) {
      console.log(`\n====== 执行测试: ${test.name} ======`);
      const passed = await test.fn();
      if (passed) {
        passedCount++;
      } else {
        failedCount++;
      }
    }

    // 输出测试结果摘要
    console.log("\n====== 测试结果摘要 ======");
    console.log(`通过: ${passedCount}`);
    console.log(`失败: ${failedCount}`);
    console.log(`总共: ${tests.length}`);

    if (failedCount === 0) {
      console.log("\n✅ 所有测试通过!");
    } else {
      console.log("\n❌ 有测试失败!");
    }

    // 关闭CDP连接
    if (cdp && cdp.client) {
      await cdp.client.close();
      console.log('已关闭CDP连接');
    }

  } catch (err) {
    console.error('测试过程中出错:', err);
  } finally {
    // 清理资源
    cleanup();
  }
}

// 如果直接运行此脚本
if (require.main === module) {
  console.log('开始运行测试...');
  console.log('当前工作目录:', process.cwd());

  main().catch(err => {
    console.error('未处理的错误:', err);
    console.error('错误堆栈:', err.stack);
    cleanup();
    process.exit(1);
  });
}