// 如果是 mac 获取整个屏幕不让获取音频要么获取标签页，要么得把下面的 configureAudio 注掉，不然会始终等待音频帧

(async function () {
  const transport = new WebTransport(
    "https://localhost:4433/webtransport/push/live/test",
    {
      // 如果是自签名证书需要获取证书哈希值，否则校验不过去
      // serverCertificateHashes: [
      //   {
      //     algorithm: "sha-256",
      //     value: new Uint8Array([
      //       158, 126, 137, 36, 236, 220, 87, 184, 67, 11, 37, 63, 235, 45, 169,
      //       235, 241, 132, 231, 16, 92, 234, 232, 178, 246, 61, 238, 79, 134,
      //       137, 249, 25,
      //     ]),
      //   },
      // ],
    }
  );
  await transport.ready;
  const transportWritable =
    (await transport.createBidirectionalStream()).writable;
  const transportWriter = transportWritable.getWriter();

  const writable = new WritableStream({
    write: (chunk) => {
      console.log(chunk);
      transportWriter.write(chunk);
    },
  });

  importScripts("flv-muxer.iife.js");

  flvMuxer = new FlvMuxer(writable, {
    mode: "record",
    chunked: false,
  });

  flvMuxer.configureVideo({
    encoderConfig: {
      codec: "avc1.640034",
      width: 2560,
      height: 1440,
      framerate: 30,
    },
    keyframeInterval: 90,
  });

  flvMuxer.configureAudio({
    encoderConfig: {
      codec: "mp4a.40.29",
      sampleRate: 44100,
      numberOfChannels: 2,
    },
  });

  self.onmessage = async (e) => {
    if (e.data.type === "DATA_VIDEO") {
      flvMuxer.addRawChunk("video", e.data.chunk);
    } else if (e.data.type == "DATA_AUDIO") {
      flvMuxer.addRawChunk("audio", e.data.chunk);
    } else if (e.data.type === "START") {
      flvMuxer.start();
    } else if (e.data.type === "PAUSE") {
      await flvMuxer.pause();
    } else if (e.data.type === "RESUME") {
      flvMuxer.resume();
    } else if (e.data.type === "STOP") {
      await flvMuxer.stop();
    }
  };
})();
