#!/usr/bin/env node

/**
 * 简化版 Cluster CDP 测试脚本
 * 此脚本用于测试 Chrome DevTools Protocol 连接和操作
 */

const CDP = require('chrome-remote-interface');

// 测试配置
const TEST_PORT = 9222;

// 一个简单的延迟函数
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));

// 连接到 CDP
async function connectToCDP() {
  try {
    console.log(`连接到 Chrome DevTools Protocol (端口 ${TEST_PORT})...`);

    // 获取可用的目标列表
    const targets = await CDP.List({ port: TEST_PORT });
    if (targets.length === 0) {
      throw new Error('没有可用的调试目标');
    }

    // 找到第一个类型为 'page' 的目标
    const target = targets.find(t => t.type === 'page');
    if (!target) {
      throw new Error('没有找到可用的页面目标');
    }

    console.log('找到目标页面:', target.title);

    // 连接到特定目标
    const client = await CDP({
      port: TEST_PORT,
      target: target
    });

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

// 测试 CDP 基本功能
async function testCDPBasics(cdp) {
  try {
    console.log('\n测试: CDP 基本功能');

    const { Runtime } = cdp;

    // 在浏览器中执行一段脚本
    const result = await Runtime.evaluate({
      expression: '2 + 2'
    });

    console.log('执行结果:', result.result.value);

    if (result.result.value === 4) {
      console.log('✅ 测试通过: CDP 执行脚本正常');
    } else {
      console.error(`❌ 测试失败: CDP 执行脚本异常，期望 4，实际 ${result.result.value}`);
      return false;
    }

    return true;
  } catch (err) {
    console.error('CDP 基本功能测试出错:', err);
    return false;
  }
}

// 测试网络请求监控
async function testNetworkMonitoring(cdp) {
  try {
    console.log('\n测试: 网络请求监控');

    const { Network, Page } = cdp;
    const requests = [];

    // 监听网络请求
    Network.requestWillBeSent((params) => {
      console.log('检测到网络请求:', params.request.url);
      requests.push(params.request.url);
    });

    console.log('正在导航到测试页面...');
    // 打开一个网页
    await Page.navigate({ url: 'https://example.com' });
    console.log('等待页面加载完成...');

    // 等待页面加载完成
    await Page.loadEventFired();
    console.log('页面加载完成，等待可能的额外请求...');

    // 等待一段时间以捕获所有请求
    await delay(3000);

    console.log(`总共捕获到 ${requests.length} 个网络请求`);

    if (requests.length > 0) {
      console.log('✅ 测试通过: 成功监控到网络请求');
      console.log('请求列表:');
      requests.forEach((url, index) => {
        console.log(`${index + 1}. ${url}`);
      });
      return true;
    } else {
      console.error('❌ 测试失败: 未能监控到任何网络请求');
      return false;
    }

  } catch (err) {
    console.error('网络请求监控测试出错:', err);
    console.error('错误详情:', err.message);
    return false;
  }
}

// 测试 DOM 操作
async function testDOMOperations(cdp) {
  try {
    console.log('\n测试: DOM 操作');

    const { Runtime, Page } = cdp;

    // 打开一个网页
    await Page.navigate({ url: 'https://example.com' });
    await Page.loadEventFired();

    // 查询页面标题
    const titleResult = await Runtime.evaluate({
      expression: 'document.title'
    });

    console.log('页面标题:', titleResult.result.value);

    if (titleResult.result.value === 'Example Domain') {
      console.log('✅ 测试通过: 成功获取页面标题');
    } else {
      console.error(`❌ 测试失败: 获取页面标题异常，期望 "Example Domain"，实际 "${titleResult.result.value}"`);
      return false;
    }

    // 修改页面元素
    await Runtime.evaluate({
      expression: 'document.querySelector("h1").textContent = "CDP 测试成功"'
    });

    // 验证修改
    const modifiedResult = await Runtime.evaluate({
      expression: 'document.querySelector("h1").textContent'
    });

    if (modifiedResult.result.value === 'CDP 测试成功') {
      console.log('✅ 测试通过: 成功修改页面元素');
    } else {
      console.error(`❌ 测试失败: 修改页面元素失败`);
      return false;
    }

    return true;
  } catch (err) {
    console.error('DOM 操作测试出错:', err);
    return false;
  }
}

// 主函数
async function main() {
  let cdp = null;

  try {
    // 连接到 CDP
    cdp = await connectToCDP();

    // 运行各种测试
    const tests = [
      { name: "CDP 基本功能", fn: () => testCDPBasics(cdp) },
      { name: "网络请求监控", fn: () => testNetworkMonitoring(cdp) },
      { name: "DOM 操作", fn: () => testDOMOperations(cdp) }
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

  } catch (err) {
    console.error('测试过程中出错:', err);
  } finally {
    // 关闭CDP连接
    if (cdp && cdp.client) {
      await cdp.client.close();
      console.log('已关闭 CDP 连接');
    }
  }
}

// 处理进程终止信号
process.on('SIGINT', async () => {
  console.log('\n接收到 SIGINT 信号，正在清理...');
  process.exit(0);
});

// 运行测试
main().catch(err => {
  console.error('未处理的错误:', err);
  process.exit(1);
}); 