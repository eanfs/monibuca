async function getDisplayMedia() {
  return navigator.mediaDevices.getDisplayMedia({
    video: {
      frameRate: {
        ideal: 30,
      },
    },
    audio: {
      numberOfChannels: 2,
      sampleRate: 44100,
    },
  });
}

let stream;

let recordingChunks = [];

const myWorker = new Worker("./worker.js");

async function startRecording() {
  myWorker.onmessage = (chunk) => {
    recordingChunks.push(chunk.data);
  };

  myWorker.postMessage({ type: "START" });

  stream = await getDisplayMedia();

  const videoTrack = stream.getVideoTracks()[0];
  const audioTrack = stream.getAudioTracks()[0];

  if (videoTrack) {
    const videoTrackProcessor = new MediaStreamTrackProcessor({
      track: videoTrack,
    });

    videoTrackProcessor.readable.pipeTo(
      new WritableStream({
        write: (chunk) => {
          myWorker.postMessage(
            {
              type: "DATA_VIDEO",
              chunk,
            },
            [chunk]
          );
        },
      })
    );
  }

  if (audioTrack) {
    const audioTrackProcessor = new MediaStreamTrackProcessor({
      track: audioTrack,
    });

    audioTrackProcessor.readable.pipeTo(
      new WritableStream({
        write: (chunk) => {
          myWorker.postMessage(
            {
              type: "DATA_AUDIO",
              chunk,
            },
            [chunk]
          );
        },
      })
    );
  }
}

function pauseRecording() {
  myWorker.postMessage({ type: "PAUSE" });
}

function resumeRecording() {
  myWorker.postMessage({ type: "RESUME" });
}

async function stopRecording() {
  try {
    myWorker.postMessage({ type: "STOP" });

    stream.getTracks().forEach((track) => {
      track.stop();
    });

    // Save the file
    const fileHandle = await window.showSaveFilePicker({
      suggestedName: "recording.flv",
      types: [
        {
          description: "FLV Video",
          accept: { "video/x-flv": [".flv"] },
        },
      ],
    });

    const writableFileStream = await fileHandle.createWritable();
    await writableFileStream.write(new Blob(recordingChunks));
    await writableFileStream.close();

    recordingChunks = [];
  } catch (error) {
    console.error("Error stopping recording:", error);
  }
}
