/*
 * WebRTC 2K 播放基线采集器（纯 JavaScript，无构建步骤）
 *
 * 每 intervalMs（默认 5000ms）对每个被接管的 RTCPeerConnection 调用 getStats()，
 * 单行输出一条 JSON 采样，字段包含：
 *   framesDecoded / fps / framesDropped / jitterBufferDelay（累计秒）/
 *   jitterBufferDelayAvg（平均秒）/ nackCount / pliCount /
 *   currentRoundTripTime / bytesReceived / packetsLost / jitter / 分辨率
 *
 * 三种用法：
 *
 * 1) 文档开始时注入（推荐，能接管页面自己创建的 pc，例如 /webrtc/test/subscribe）：
 *    - Playwright: context.addInitScript({ content: 本文件内容 }) 后再 goto 播放页；
 *    - 浏览器用户脚本（Tampermonkey 等）设为 document-start 执行。
 *
 * 2) 浏览器控制台直接粘贴（不依赖页面里的 pc 变量）：
 *    粘贴执行后，在控制台调用
 *      __webrtc2kCollector.subscribe('live/2k')
 *    采集器会自己创建 RTCPeerConnection 走 /webrtc/play/{streamPath} 信令并播放。
 *    也可以对已有对象显式接管：
 *      __webrtc2kCollector.attach(pc)
 *    注意：控制台粘贴的代码不会在页面刷新后保留；若要让播放页自己创建的 pc
 *    被自动接管，必须用第 1 种“文档开始时注入”的方式。
 *
 * 3) 结束采集：
 *      __webrtc2kCollector.stop('手工停止')
 *    会打印一行 summary JSON 并返回该对象；采样明细可用
 *      __webrtc2kCollector.getRecords()
 *    取回。也可在注入前设置 window.__WEBRTC_2K_DURATION_MS__ = 90000 自动停止。
 *
 * 可配置的全局变量（在脚本执行前设置）：
 *   window.__WEBRTC_2K_INTERVAL_MS__  采样间隔，默认 5000
 *   window.__WEBRTC_2K_DURATION_MS__  自动停止时长，默认 0（不自动停止）
 *   window.__WEBRTC_2K_LABEL__        采样里的标签，默认空
 */
(function () {
  'use strict';

  var LOG_PREFIX = '[webrtc-2k]';

  if (typeof window === 'undefined' || typeof window.RTCPeerConnection !== 'function') {
    if (typeof console !== 'undefined') {
      console.warn(LOG_PREFIX + ' 当前环境没有 RTCPeerConnection，采集器未安装');
    }
    return;
  }
  if (window.__webrtc2kCollector) {
    console.info(LOG_PREFIX + ' 采集器已安装，跳过重复注入');
    return;
  }

  function readNumber(value, fallback) {
    var n = Number(value);
    return isFinite(n) && n >= 0 ? n : fallback;
  }

  var intervalMs = readNumber(window.__WEBRTC_2K_INTERVAL_MS__, 5000);
  if (intervalMs <= 0) intervalMs = 5000;
  var durationMs = readNumber(window.__WEBRTC_2K_DURATION_MS__, 0);
  var label = typeof window.__WEBRTC_2K_LABEL__ === 'string' ? window.__WEBRTC_2K_LABEL__ : '';

  function nowMs() {
    return window.performance && typeof window.performance.now === 'function'
      ? window.performance.now()
      : Date.now();
  }

  function round(value, digits) {
    if (typeof value !== 'number' || !isFinite(value)) return null;
    var factor = Math.pow(10, digits);
    return Math.round(value * factor) / factor;
  }

  function numberOrNull(value) {
    return typeof value === 'number' && isFinite(value) ? value : null;
  }

  function deltaOrNull(current, previous) {
    return typeof current === 'number' && typeof previous === 'number'
      ? current - previous
      : null;
  }

  var state = {
    startedAt: nowMs(),
    records: [],
    handles: [],
    timerId: null,
    stopTimerId: null,
    sampling: false,
    stopped: false,
    lastSummary: null,
    errors: 0
  };

  function findInboundVideo(report) {
    var chosen = null;
    report.forEach(function (stat) {
      if (!stat || stat.type !== 'inbound-rtp') return;
      var kind = stat.kind || stat.mediaType;
      if (kind !== 'video') return;
      if (typeof stat.framesDecoded !== 'number' && typeof stat.bytesReceived !== 'number') return;
      if (!chosen || (stat.bytesReceived || 0) > (chosen.bytesReceived || 0)) {
        chosen = stat;
      }
    });
    return chosen;
  }

  function findCandidatePair(report) {
    var best = null;
    var bestScore = -1;
    report.forEach(function (stat) {
      if (!stat || stat.type !== 'candidate-pair') return;
      if (typeof stat.currentRoundTripTime !== 'number') return;
      var score = 0;
      if (stat.state === 'succeeded') score += 1;
      if (stat.nominated) score += 2;
      if (score > bestScore) {
        best = stat;
        bestScore = score;
      }
    });
    return best;
  }

  function buildSample(handle, inbound, pair, tNow) {
    var prev = handle.last;
    var decoded = numberOrNull(inbound.framesDecoded);
    var dropped = numberOrNull(inbound.framesDropped);
    var nack = numberOrNull(inbound.nackCount);
    var pli = numberOrNull(inbound.pliCount);
    var jbDelay = numberOrNull(inbound.jitterBufferDelay);
    var jbEmitted = numberOrNull(inbound.jitterBufferEmittedCount);

    var spanSec = prev ? (tNow - prev.t) / 1000 : null;
    var fps = null;
    if (prev && spanSec > 0 && typeof decoded === 'number' && typeof prev.decoded === 'number') {
      fps = (decoded - prev.decoded) / spanSec;
    }

    var sample = {
      type: 'sample',
      label: label || null,
      iso: new Date().toISOString(),
      pc: handle.id,
      connectionState: handle.pc.connectionState,
      elapsedSec: round((tNow - state.startedAt) / 1000, 3),
      width: numberOrNull(inbound.frameWidth),
      height: numberOrNull(inbound.frameHeight),
      framesDecoded: decoded,
      fps: round(fps, 2),
      framesPerSecond: numberOrNull(inbound.framesPerSecond),
      framesDropped: dropped,
      framesDroppedDelta: deltaOrNull(dropped, prev && prev.dropped),
      jitterBufferDelay: jbDelay,
      jitterBufferDelayAvg: jbDelay !== null && jbEmitted ? round(jbDelay / jbEmitted, 4) : null,
      nackCount: nack,
      nackCountDelta: deltaOrNull(nack, prev && prev.nack),
      pliCount: pli,
      pliCountDelta: deltaOrNull(pli, prev && prev.pli),
      packetsLost: numberOrNull(inbound.packetsLost),
      bytesReceived: numberOrNull(inbound.bytesReceived),
      jitter: numberOrNull(inbound.jitter),
      currentRoundTripTime: pair ? numberOrNull(pair.currentRoundTripTime) : null
    };

    handle.last = { t: tNow, decoded: decoded, dropped: dropped, nack: nack, pli: pli };
    handle.history.push({
      t: tNow,
      decoded: decoded,
      dropped: dropped,
      nack: nack,
      pli: pli,
      fps: sample.fps,
      jitterBufferDelayAvg: sample.jitterBufferDelayAvg,
      rtt: sample.currentRoundTripTime
    });
    return sample;
  }

  async function collectOnce() {
    if (state.stopped || state.sampling) return;
    state.sampling = true;
    try {
      var handles = state.handles.slice();
      for (var i = 0; i < handles.length; i += 1) {
        var handle = handles[i];
        if (handle.pc.connectionState === 'closed') continue;
        var report;
        try {
          report = await handle.pc.getStats();
        } catch (error) {
          state.errors += 1;
          console.warn(LOG_PREFIX + ' getStats 失败 pc=' + handle.id + ': ' + error);
          continue;
        }
        var inbound = findInboundVideo(report);
        if (!inbound) continue;
        var sample = buildSample(handle, inbound, findCandidatePair(report), nowMs());
        state.records.push(sample);
        console.log(LOG_PREFIX + ' ' + JSON.stringify(sample));
      }
    } finally {
      state.sampling = false;
    }
  }

  function summarizeHandle(handle) {
    var history = handle.history;
    if (history.length === 0) {
      return { pc: handle.id, connectionState: handle.pc.connectionState, samples: 0 };
    }
    var first = history[0];
    var last = history[history.length - 1];
    var spanSec = (last.t - first.t) / 1000;
    var decodedDelta = deltaOrNull(last.decoded, first.decoded);
    var fpsValues = [];
    var jbValues = [];
    for (var i = 0; i < history.length; i += 1) {
      if (typeof history[i].fps === 'number') fpsValues.push(history[i].fps);
      if (typeof history[i].jitterBufferDelayAvg === 'number') jbValues.push(history[i].jitterBufferDelayAvg);
    }
    var sum = function (values) {
      var total = 0;
      for (var j = 0; j < values.length; j += 1) total += values[j];
      return total;
    };
    return {
      pc: handle.id,
      connectionState: handle.pc.connectionState,
      samples: history.length,
      spanSec: round(spanSec, 2),
      decodedFrames: decodedDelta,
      avgFps: spanSec > 0 && decodedDelta !== null ? round(decodedDelta / spanSec, 2) : null,
      sampleFpsMean: fpsValues.length ? round(sum(fpsValues) / fpsValues.length, 2) : null,
      sampleFpsMin: fpsValues.length ? round(Math.min.apply(null, fpsValues), 2) : null,
      framesDropped: deltaOrNull(last.dropped, first.dropped),
      nackCount: deltaOrNull(last.nack, first.nack),
      pliCount: deltaOrNull(last.pli, first.pli),
      jitterBufferDelayAvg: jbValues.length ? round(sum(jbValues) / jbValues.length, 4) : null,
      lastRtt: last.rtt
    };
  }

  function summarize() {
    var pcs = [];
    for (var i = 0; i < state.handles.length; i += 1) {
      pcs.push(summarizeHandle(state.handles[i]));
    }
    return {
      type: 'summary',
      label: label || null,
      startedAt: new Date(new Date().getTime() - (nowMs() - state.startedAt)).toISOString(),
      elapsedSec: round((nowMs() - state.startedAt) / 1000, 2),
      intervalMs: intervalMs,
      samples: state.records.length,
      errors: state.errors,
      peerConnections: pcs
    };
  }

  function stop(reason) {
    if (state.stopped) return state.lastSummary;
    state.stopped = true;
    if (state.timerId !== null) {
      clearInterval(state.timerId);
      state.timerId = null;
    }
    if (state.stopTimerId !== null) {
      clearTimeout(state.stopTimerId);
      state.stopTimerId = null;
    }
    state.lastSummary = summarize();
    state.lastSummary.reason = reason || 'manual';
    console.log(LOG_PREFIX + ' summary ' + JSON.stringify(state.lastSummary));
    return state.lastSummary;
  }

  function resetSamplingTimer() {
    if (state.timerId !== null) clearInterval(state.timerId);
    state.timerId = setInterval(function () {
      collectOnce();
    }, intervalMs);
  }

  function attach(pc) {
    if (!pc || typeof pc.getStats !== 'function') {
      console.warn(LOG_PREFIX + ' attach 失败：不是有效的 RTCPeerConnection');
      return null;
    }
    for (var i = 0; i < state.handles.length; i += 1) {
      if (state.handles[i].pc === pc) return state.handles[i];
    }
    var handle = {
      id: state.handles.length + 1,
      pc: pc,
      last: null,
      history: []
    };
    state.handles.push(handle);
    console.info(LOG_PREFIX + ' 已接管 PeerConnection #' + handle.id + '（采样间隔 ' + intervalMs + 'ms）');
    pc.addEventListener('connectionstatechange', function () {
      console.info(LOG_PREFIX + ' pc#' + handle.id + ' connectionState=' + pc.connectionState);
    });
    // 先取一个基线样本，之后按间隔采样，首个区间就能算出 fps
    setTimeout(function () {
      collectOnce();
    }, 0);
    if (state.timerId === null && !state.stopped) resetSamplingTimer();
    return handle;
  }

  // 接管页面之后创建的 RTCPeerConnection（必须在本脚本于文档开始时执行时才有效）
  var NativeRTCPeerConnection = window.RTCPeerConnection;
  function ManagedRTCPeerConnection() {
    var args = Array.prototype.slice.call(arguments);
    var pc;
    if (typeof Reflect !== 'undefined' && typeof Reflect.construct === 'function') {
      pc = Reflect.construct(NativeRTCPeerConnection, args, NativeRTCPeerConnection);
    } else {
      pc = new (Function.prototype.bind.apply(NativeRTCPeerConnection, [null].concat(args)))();
    }
    attach(pc);
    return pc;
  }
  ManagedRTCPeerConnection.prototype = NativeRTCPeerConnection.prototype;
  if (typeof Object.setPrototypeOf === 'function') {
    Object.setPrototypeOf(ManagedRTCPeerConnection, NativeRTCPeerConnection);
  }
  window.RTCPeerConnection = ManagedRTCPeerConnection;

  // 控制台用法：自己没有 pc 对象时，用这个函数创建并播放一路流。
  async function subscribe(streamPath, options) {
    var opts = options || {};
    var path = streamPath || opts.streamPath || 'live/2k';
    if (state.stopped) {
      console.warn(LOG_PREFIX + ' 采集器已 stop，请刷新页面后重新注入');
      return null;
    }
    var pc = new window.RTCPeerConnection();
    pc.addTransceiver('video', { direction: 'recvonly' });
    if (opts.audio !== false) {
      pc.addTransceiver('audio', { direction: 'recvonly' });
    }
    var video = null;
    if (opts.video !== false) {
      video = document.createElement('video');
      video.autoplay = true;
      video.muted = true;
      video.playsInline = true;
      video.controls = true;
      video.style.width = '100%';
      video.id = 'webrtc-2k-video';
      // 必须在 setRemoteDescription 之前注册，避免 track 事件抢跑
      pc.ontrack = function (event) {
        if (event.streams && event.streams.length > 0) {
          video.srcObject = event.streams[0];
          var playing = video.play();
          if (playing && typeof playing.catch === 'function') {
            playing.catch(function (error) {
              console.warn(LOG_PREFIX + ' video.play() 被拒绝: ' + error);
            });
          }
        }
      };
      document.body.appendChild(video);
      window.__webrtc2kVideo = video;
    }
    var offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    var response = await fetch((opts.baseUrl || '') + '/webrtc/play/' + path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/sdp' },
      body: offer.sdp
    });
    if (!response.ok) {
      throw new Error('信令请求失败：HTTP ' + response.status + ' ' + (await response.text()).slice(0, 200));
    }
    var answerSdp = await response.text();
    await pc.setRemoteDescription({ type: 'answer', sdp: answerSdp });
    console.info(LOG_PREFIX + ' 已发起订阅 ' + path + '，等待媒体流');
    return pc;
  }

  function getRecords() {
    return state.records.slice();
  }

  window.__webrtc2kCollector = {
    state: state,
    attach: attach,
    subscribe: subscribe,
    stop: stop,
    getRecords: getRecords,
    nativeRTCPeerConnection: NativeRTCPeerConnection,
    intervalMs: intervalMs
  };

  if (durationMs > 0) {
    state.stopTimerId = setTimeout(function () {
      stop('duration ' + durationMs + 'ms reached');
    }, durationMs);
  }

  console.info(LOG_PREFIX + ' 采集器已安装：间隔 ' + intervalMs + 'ms' +
    (durationMs > 0 ? '，自动停止 ' + durationMs + 'ms' : '') +
    (label ? '，标签 ' + label : '') +
    '。停止：__webrtc2kCollector.stop()');
})();
