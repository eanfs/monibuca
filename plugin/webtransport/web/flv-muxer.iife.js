var FlvMuxer = (function () {
    'use strict';

    /******************************************************************************
    Copyright (c) Microsoft Corporation.

    Permission to use, copy, modify, and/or distribute this software for any
    purpose with or without fee is hereby granted.

    THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
    REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
    AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
    INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
    LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR
    OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
    PERFORMANCE OF THIS SOFTWARE.
    ***************************************************************************** */
    /* global Reflect, Promise, SuppressedError, Symbol, Iterator */


    function __classPrivateFieldGet(receiver, state, kind, f) {
        if (kind === "a" && !f) throw new TypeError("Private accessor was defined without a getter");
        if (typeof state === "function" ? receiver !== state || !f : !state.has(receiver)) throw new TypeError("Cannot read private member from an object whose class did not declare it");
        return kind === "m" ? f : kind === "a" ? f.call(receiver) : f ? f.value : state.get(receiver);
    }

    function __classPrivateFieldSet(receiver, state, value, kind, f) {
        if (kind === "m") throw new TypeError("Private method is not writable");
        if (kind === "a" && !f) throw new TypeError("Private accessor was defined without a setter");
        if (typeof state === "function" ? receiver !== state || !f : !state.has(receiver)) throw new TypeError("Cannot write private member to an object whose class did not declare it");
        return (kind === "a" ? f.call(receiver, value) : f ? f.value = value : state.set(receiver, value)), value;
    }

    typeof SuppressedError === "function" ? SuppressedError : function (error, suppressed, message) {
        var e = new Error(message);
        return e.name = "SuppressedError", e.error = error, e.suppressed = suppressed, e;
    };

    class AACRawStrategy {
        process(chunk, encoder) {
            return encoder.encodeAudioTag({
                aacPacketType: "AACRaw",
                audioData: chunk.data,
                soundFormat: "AAC",
                soundRate: "kHz44",
                soundSize: "Sound16bit",
                soundType: "Stereo",
                timestamp: chunk.timestamp,
            });
        }
    }
    class AACSEStrategy {
        process(chunk, encoder) {
            return encoder.encodeAudioTag({
                aacPacketType: "AACSequenceHeader",
                audioData: chunk.data,
                soundFormat: "AAC",
                soundRate: "kHz44",
                soundSize: "Sound16bit",
                soundType: "Stereo",
                timestamp: 0,
            });
        }
    }
    class AVCSEStrategy {
        process(chunk, encoder) {
            return encoder.encodeVideoTag("KeyFrame", "AVC", "SequenceHeader", 0, 0, chunk.data);
        }
    }
    class AVCNALUStrategy {
        process(chunk, encoder) {
            return encoder.encodeVideoTag(chunk.isKey ? "KeyFrame" : "InterFrame", "AVC", "NALU", 0, chunk.timestamp, chunk.data);
        }
    }

    /**
     * Logger 类用于处理不同日志级别的日志记录。
     */
    class Logger {
        /**
         * 设置日志记录器的日志级别。
         * @param level - 要设置的日志级别。
         */
        static setLogLevel(level) {
            this.logLevel = level;
        }
        /**
         * 确定给定日志级别的消息是否应被记录。
         * @param level - 消息的日志级别。
         * @returns 一个布尔值，指示消息是否应被记录。
         */
        static canLog(level) {
            const levels = ['debug', 'info', 'warn', 'error'];
            return levels.indexOf(level) >= levels.indexOf(this.logLevel);
        }
        /**
         * 使用时间戳和日志级别将日志消息打印到控制台。
         * @param level - 消息的日志级别。
         * @param message - 要记录的消息。
         */
        static printMessage(level, message) {
            const timestamp = new Date().toISOString();
            console[level](`[${timestamp}] [${level.toUpperCase()}] ${message}`);
        }
        /**
         * 记录调试消息。
         * @param message - 要记录的调试消息。
         */
        static debug(message) {
            if (this.canLog('debug')) {
                this.printMessage('debug', message);
            }
        }
        /**
         * 记录信息性消息。
         * @param message - 要记录的信息性消息。
         */
        static info(message) {
            if (this.canLog('info')) {
                this.printMessage('info', message);
            }
        }
        /**
         * 记录警告消息。
         * @param message - 要记录的警告消息。
         */
        static warn(message) {
            if (this.canLog('warn')) {
                this.printMessage('warn', message);
            }
        }
        /**
         * 记录错误消息。
         * @param message - 要记录的错误消息。
         */
        static error(message) {
            if (this.canLog('error')) {
                this.printMessage('error', message);
            }
        }
    }
    /**
     * 当前日志记录器的日志级别。
     * @default 'debug'
     */
    Object.defineProperty(Logger, "logLevel", {
        enumerable: true,
        configurable: true,
        writable: true,
        value: 'debug'
    });

    var _a$1, _EventBus_instance, _EventBus_events;
    class EventBus {
        constructor() {
            _EventBus_events.set(this, void 0);
            __classPrivateFieldSet(this, _EventBus_events, new Map(), "f");
        }
        static getInstance() {
            if (!__classPrivateFieldGet(this, _a$1, "f", _EventBus_instance)) {
                __classPrivateFieldSet(this, _a$1, new _a$1(), "f", _EventBus_instance);
            }
            return __classPrivateFieldGet(this, _a$1, "f", _EventBus_instance);
        }
        on(eventName, handler) {
            if (!__classPrivateFieldGet(this, _EventBus_events, "f").has(eventName)) {
                __classPrivateFieldGet(this, _EventBus_events, "f").set(eventName, []);
            }
            __classPrivateFieldGet(this, _EventBus_events, "f").get(eventName).push(handler);
        }
        off(eventName, handler) {
            if (!__classPrivateFieldGet(this, _EventBus_events, "f").has(eventName))
                return;
            const handlers = __classPrivateFieldGet(this, _EventBus_events, "f").get(eventName);
            const index = handlers.indexOf(handler);
            if (index !== -1) {
                handlers.splice(index, 1);
            }
            if (handlers.length === 0) {
                __classPrivateFieldGet(this, _EventBus_events, "f").delete(eventName);
            }
        }
        once(eventName, handler) {
            const onceHandler = (...args) => {
                handler(...args);
                this.off(eventName, onceHandler);
            };
            this.on(eventName, onceHandler);
        }
        emit(eventName, ...args) {
            if (!__classPrivateFieldGet(this, _EventBus_events, "f").has(eventName))
                return;
            const handlers = __classPrivateFieldGet(this, _EventBus_events, "f").get(eventName);
            handlers.forEach((handler) => {
                try {
                    handler(...args);
                }
                catch (error) {
                    Logger.error(`Error in event handler for ${eventName}: ${error}`);
                }
            });
        }
        clear() {
            __classPrivateFieldGet(this, _EventBus_events, "f").clear();
        }
    }
    _a$1 = EventBus, _EventBus_events = new WeakMap();
    _EventBus_instance = { value: void 0 };

    var _StreamProcessor_instances, _StreamProcessor_eventBus, _StreamProcessor_audioEncoderTrack, _StreamProcessor_videoEncoderTrack, _StreamProcessor_audioConfigReady, _StreamProcessor_videoConfigReady, _StreamProcessor_initListeners, _StreamProcessor_flush, _StreamProcessor_processAudioChunk, _StreamProcessor_processVideoChunk, _StreamProcessor_publishChunk;
    class StreamProcessor {
        constructor() {
            _StreamProcessor_instances.add(this);
            Object.defineProperty(this, "state", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: "inactive"
            });
            _StreamProcessor_eventBus.set(this, void 0);
            _StreamProcessor_audioEncoderTrack.set(this, void 0);
            _StreamProcessor_videoEncoderTrack.set(this, void 0);
            _StreamProcessor_audioConfigReady.set(this, false);
            _StreamProcessor_videoConfigReady.set(this, false);
            __classPrivateFieldSet(this, _StreamProcessor_eventBus, EventBus.getInstance(), "f");
            __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_initListeners).call(this);
        }
        setAudioConfigReady() {
            __classPrivateFieldSet(this, _StreamProcessor_audioConfigReady, true, "f");
        }
        setVideoConfigReady() {
            __classPrivateFieldSet(this, _StreamProcessor_videoConfigReady, true, "f");
        }
        static getInstance() {
            if (!this.instance) {
                this.instance = new this();
            }
            return this.instance;
        }
        addTrackChunk(type, chunk) {
            if (this.state !== "recording") {
                chunk.close();
                return;
            }
            if (type === "audio") {
                __classPrivateFieldGet(this, _StreamProcessor_audioEncoderTrack, "f")?.addTrackChunk(chunk);
            }
            else if (type === "video") {
                __classPrivateFieldGet(this, _StreamProcessor_videoEncoderTrack, "f")?.addTrackChunk(chunk);
            }
        }
        addAudioTrack(track) {
            __classPrivateFieldSet(this, _StreamProcessor_audioEncoderTrack, track, "f");
        }
        addVideoTrack(track) {
            __classPrivateFieldSet(this, _StreamProcessor_videoEncoderTrack, track, "f");
        }
        handleTrackChunk(chunk) {
            // 如果只有单个轨道，则发出该数据块
            if (!__classPrivateFieldGet(this, _StreamProcessor_audioEncoderTrack, "f") || !__classPrivateFieldGet(this, _StreamProcessor_videoEncoderTrack, "f")) {
                __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_publishChunk).call(this, chunk);
                return;
            }
            if (chunk.type === "AAC_SE") {
                if (!__classPrivateFieldGet(this, _StreamProcessor_audioConfigReady, "f")) {
                    this.setAudioConfigReady();
                }
                __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_publishChunk).call(this, chunk);
                return;
            }
            if (chunk.type === "AVC_SE") {
                if (!__classPrivateFieldGet(this, _StreamProcessor_videoConfigReady, "f")) {
                    this.setVideoConfigReady();
                }
                __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_publishChunk).call(this, chunk);
                return;
            }
            // 如果是音频包
            if (chunk.type === "AAC_RAW") {
                __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_processAudioChunk).call(this, chunk);
                return;
            }
            // 如果是视频包
            if (chunk.type === "AVC_NALU") {
                __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_processVideoChunk).call(this, chunk);
                return;
            }
        }
        start() {
            if (this.state !== "inactive") {
                return;
            }
            this.state = "recording";
        }
        async pause() {
            if (this.state !== "recording") {
                return;
            }
            await __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_flush).call(this);
            this.state = "paused";
        }
        resume() {
            if (this.state !== "paused") {
                return;
            }
            this.state = "recording";
        }
        async stop() {
            if (this.state !== "recording") {
                return;
            }
            await __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_flush).call(this);
            this.reset();
            this.state = "inactive";
        }
        reset() {
            __classPrivateFieldGet(this, _StreamProcessor_videoEncoderTrack, "f")?.reset();
            __classPrivateFieldSet(this, _StreamProcessor_videoConfigReady, false, "f");
            __classPrivateFieldGet(this, _StreamProcessor_audioEncoderTrack, "f")?.reset();
            __classPrivateFieldSet(this, _StreamProcessor_audioConfigReady, false, "f");
        }
    }
    _StreamProcessor_eventBus = new WeakMap(), _StreamProcessor_audioEncoderTrack = new WeakMap(), _StreamProcessor_videoEncoderTrack = new WeakMap(), _StreamProcessor_audioConfigReady = new WeakMap(), _StreamProcessor_videoConfigReady = new WeakMap(), _StreamProcessor_instances = new WeakSet(), _StreamProcessor_initListeners = function _StreamProcessor_initListeners() {
        __classPrivateFieldGet(this, _StreamProcessor_eventBus, "f").on("TRACK_CHUNK", (chunk) => {
            this.handleTrackChunk(chunk);
        });
    }, _StreamProcessor_flush = async function _StreamProcessor_flush() {
        await __classPrivateFieldGet(this, _StreamProcessor_audioEncoderTrack, "f")?.flush();
        await __classPrivateFieldGet(this, _StreamProcessor_videoEncoderTrack, "f")?.flush();
    }, _StreamProcessor_processAudioChunk = function _StreamProcessor_processAudioChunk(chunk) {
        const audioTrack = __classPrivateFieldGet(this, _StreamProcessor_audioEncoderTrack, "f");
        const videoTrack = __classPrivateFieldGet(this, _StreamProcessor_videoEncoderTrack, "f");
        if (!__classPrivateFieldGet(this, _StreamProcessor_audioConfigReady, "f") || !__classPrivateFieldGet(this, _StreamProcessor_videoConfigReady, "f")) {
            audioTrack.enqueue(chunk);
            return;
        }
        while (!videoTrack.isEmpty() &&
            videoTrack.peek().timestamp <= chunk.timestamp) {
            __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_publishChunk).call(this, videoTrack.dequeue());
        }
        if (chunk.timestamp <= videoTrack.lastTimestamp) {
            __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_publishChunk).call(this, chunk);
        }
        else {
            audioTrack.enqueue(chunk);
        }
    }, _StreamProcessor_processVideoChunk = function _StreamProcessor_processVideoChunk(chunk) {
        const audioTrack = __classPrivateFieldGet(this, _StreamProcessor_audioEncoderTrack, "f");
        const videoTrack = __classPrivateFieldGet(this, _StreamProcessor_videoEncoderTrack, "f");
        if (!__classPrivateFieldGet(this, _StreamProcessor_audioConfigReady, "f") || !__classPrivateFieldGet(this, _StreamProcessor_videoConfigReady, "f")) {
            videoTrack.enqueue(chunk);
            return;
        }
        while (!audioTrack.isEmpty() &&
            audioTrack.peek().timestamp <= chunk.timestamp) {
            __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_publishChunk).call(this, audioTrack.dequeue());
        }
        if (chunk.timestamp <= audioTrack.lastTimestamp) {
            __classPrivateFieldGet(this, _StreamProcessor_instances, "m", _StreamProcessor_publishChunk).call(this, chunk);
        }
        else {
            videoTrack.enqueue(chunk);
        }
    }, _StreamProcessor_publishChunk = function _StreamProcessor_publishChunk(chunk) {
        if (!chunk)
            return;
        __classPrivateFieldGet(this, _StreamProcessor_eventBus, "f").emit("CHUNK_PUBLISH", chunk);
    };

    var _a, _TrackState_instance, _VideoEncoderTrack_frameCount, _VideoEncoderTrack_keyframeInterval;
    class TrackState {
        constructor() {
            Object.defineProperty(this, "offsetTimestamp", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: 0
            });
        }
        static getInstance() {
            if (!__classPrivateFieldGet(this, _a, "f", _TrackState_instance)) {
                __classPrivateFieldSet(this, _a, new this(), "f", _TrackState_instance);
            }
            return __classPrivateFieldGet(this, _a, "f", _TrackState_instance);
        }
    }
    _a = TrackState;
    _TrackState_instance = { value: void 0 };
    class BaseEncoderTrack {
        get decoderConfig() {
            return this._decoderConfig;
        }
        get state() {
            return StreamProcessor.getInstance().state;
        }
        set decoderConfig(config) {
            if (this._decoderConfig)
                return;
            this._decoderConfig = config;
            if (this instanceof AudioEncoderTrack) {
                this.eventBus.emit("TRACK_CHUNK", {
                    type: "AAC_SE",
                    data: new Uint8Array(config.description),
                    timestamp: 0,
                    isKey: true,
                });
            }
            else {
                this.eventBus.emit("TRACK_CHUNK", {
                    type: "AVC_SE",
                    data: new Uint8Array(config.description),
                    timestamp: 0,
                    isKey: true,
                });
            }
        }
        constructor(config) {
            Object.defineProperty(this, "eventBus", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: void 0
            });
            Object.defineProperty(this, "mode", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: "record"
            });
            Object.defineProperty(this, "config", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: void 0
            });
            Object.defineProperty(this, "queue", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: []
            });
            Object.defineProperty(this, "encoder", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: void 0
            });
            Object.defineProperty(this, "trackState", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: void 0
            });
            Object.defineProperty(this, "lastTimestamp", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: 0
            });
            Object.defineProperty(this, "_decoderConfig", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: void 0
            });
            this.eventBus = EventBus.getInstance();
            this.trackState = TrackState.getInstance();
            this.config = config;
            this.initEncoder(config);
        }
        enqueue(chunk) {
            this.queue.push(chunk);
        }
        dequeue() {
            return this.queue.shift();
        }
        peek() {
            return this.queue[0];
        }
        isEmpty() {
            return this.queue.length === 0;
        }
        length() {
            return this.queue.length;
        }
        close() {
            if (this.state !== "recording") {
                throw new Error("Cannot stop track as it is not currently recording.");
            }
            if (!this.encoder) {
                throw new Error("Encoder is not initialized.");
            }
            this.encoder.close();
        }
        reset() {
            if (this.trackState.offsetTimestamp !== 0) {
                this.trackState.offsetTimestamp = 0;
            }
            this._decoderConfig = undefined;
        }
        async flush() {
            if (!this.encoder) {
                throw new Error("Encoder is not initialized.");
            }
            return this.encoder.flush();
        }
        calculateTimestamp(timestamp) {
            if (!this.trackState.offsetTimestamp) {
                this.trackState.offsetTimestamp = timestamp;
            }
            return Math.max(0, (timestamp - this.trackState.offsetTimestamp) / 1000);
        }
    }
    class VideoEncoderTrack extends BaseEncoderTrack {
        constructor(config, keyframeInterval) {
            super(config);
            _VideoEncoderTrack_frameCount.set(this, 0);
            _VideoEncoderTrack_keyframeInterval.set(this, 90);
            __classPrivateFieldSet(this, _VideoEncoderTrack_keyframeInterval, keyframeInterval, "f");
        }
        initEncoder(config) {
            this.encoder = new VideoEncoder({
                output: this.handleOutput.bind(this),
                error: (error) => {
                    Logger.error(`VideoEncoder error: ${error}`);
                    throw new Error(error.message);
                },
            });
            this.encoder.configure(config);
        }
        addTrackChunk(frame) {
            var _b;
            if (this.state !== "recording") {
                frame.close();
                return;
            }
            if (!this.encoder) {
                throw new Error("Encoder is not initialized.");
            }
            this.encoder.encode(frame, {
                keyFrame: __classPrivateFieldGet(this, _VideoEncoderTrack_frameCount, "f") % __classPrivateFieldGet(this, _VideoEncoderTrack_keyframeInterval, "f") === 0,
            });
            __classPrivateFieldSet(this, _VideoEncoderTrack_frameCount, (_b = __classPrivateFieldGet(this, _VideoEncoderTrack_frameCount, "f"), _b++, _b), "f");
            frame.close();
        }
        handleOutput(chunk, metadata) {
            try {
                // 添加视频元数据到缓冲区
                if (metadata?.decoderConfig?.description) {
                    this.decoderConfig = metadata.decoderConfig;
                }
                // 将 EncodedVideoChunk 转为 Uint8Array
                const data = new Uint8Array(chunk.byteLength);
                chunk.copyTo(data);
                // 添加视频数据到缓冲区
                const timestamp = this.calculateTimestamp(chunk.timestamp);
                this.eventBus.emit("TRACK_CHUNK", {
                    type: "AVC_NALU",
                    data,
                    timestamp,
                    isKey: chunk.type === "key",
                });
                this.lastTimestamp = timestamp;
            }
            catch (error) {
                Logger.error(`Failed to handle video chunk: ${error}`);
            }
        }
        reset() {
            super.reset();
            __classPrivateFieldSet(this, _VideoEncoderTrack_frameCount, 0, "f");
        }
    }
    _VideoEncoderTrack_frameCount = new WeakMap(), _VideoEncoderTrack_keyframeInterval = new WeakMap();
    class AudioEncoderTrack extends BaseEncoderTrack {
        initEncoder(config) {
            this.encoder = new AudioEncoder({
                output: this.handleOutput.bind(this),
                error: (error) => {
                    Logger.error(`AudioEncoder error:", ${error.message} `);
                    throw new Error(error.message);
                },
            });
            this.encoder.configure(config);
        }
        addTrackChunk(chunk) {
            if (this.state !== "recording") {
                chunk.close();
                return;
            }
            if (!this.encoder) {
                throw new Error("Encoder is not initialized.");
            }
            this.encoder.encode(chunk);
            chunk.close();
        }
        handleOutput(chunk, metadata) {
            try {
                // 如果是关键帧，则添加音频解码器配置
                if (metadata?.decoderConfig?.description) {
                    this.decoderConfig = metadata.decoderConfig;
                }
                // 将 EncodedAudioChunk 转为 Uint8Array
                const data = new Uint8Array(chunk.byteLength);
                chunk.copyTo(data);
                // 添加音频数据到缓冲区
                const timestamp = super.calculateTimestamp(chunk.timestamp); // 转换成相对时间
                this.eventBus.emit("TRACK_CHUNK", {
                    type: "AAC_RAW",
                    data,
                    timestamp,
                    isKey: chunk.type === "key",
                });
                this.lastTimestamp = timestamp;
            }
            catch (error) {
                Logger.error(`Failed to handle audio chunk: ${error}`);
            }
        }
    }

    var SoundFormat;
    (function (SoundFormat) {
        SoundFormat[SoundFormat["LinearPCMPlatformEndian"] = 0] = "LinearPCMPlatformEndian";
        SoundFormat[SoundFormat["ADPCM"] = 1] = "ADPCM";
        SoundFormat[SoundFormat["MP3"] = 2] = "MP3";
        SoundFormat[SoundFormat["LinearPCMLittleEndian"] = 3] = "LinearPCMLittleEndian";
        SoundFormat[SoundFormat["Nellymoser16kHzMono"] = 4] = "Nellymoser16kHzMono";
        SoundFormat[SoundFormat["Nellymoser8kHzMono"] = 5] = "Nellymoser8kHzMono";
        SoundFormat[SoundFormat["Nellymoser"] = 6] = "Nellymoser";
        SoundFormat[SoundFormat["G711ALawLogarithmicPCM"] = 7] = "G711ALawLogarithmicPCM";
        SoundFormat[SoundFormat["G711MuLawLogarithmicPCM"] = 8] = "G711MuLawLogarithmicPCM";
        SoundFormat[SoundFormat["Reserved"] = 9] = "Reserved";
        SoundFormat[SoundFormat["AAC"] = 10] = "AAC";
        SoundFormat[SoundFormat["Speex"] = 11] = "Speex";
        SoundFormat[SoundFormat["MP38kHz"] = 14] = "MP38kHz";
        SoundFormat[SoundFormat["DeviceSpecificSound"] = 15] = "DeviceSpecificSound";
    })(SoundFormat || (SoundFormat = {}));
    var SoundRate;
    (function (SoundRate) {
        SoundRate[SoundRate["kHz5_5"] = 0] = "kHz5_5";
        SoundRate[SoundRate["kHz11"] = 1] = "kHz11";
        SoundRate[SoundRate["kHz22"] = 2] = "kHz22";
        SoundRate[SoundRate["kHz44"] = 3] = "kHz44";
    })(SoundRate || (SoundRate = {}));
    var SoundSize;
    (function (SoundSize) {
        SoundSize[SoundSize["Sound8bit"] = 0] = "Sound8bit";
        SoundSize[SoundSize["Sound16bit"] = 1] = "Sound16bit";
    })(SoundSize || (SoundSize = {}));
    var SoundType;
    (function (SoundType) {
        SoundType[SoundType["Mono"] = 0] = "Mono";
        SoundType[SoundType["Stereo"] = 1] = "Stereo";
    })(SoundType || (SoundType = {}));
    var AACPacketType;
    (function (AACPacketType) {
        AACPacketType[AACPacketType["AACSequenceHeader"] = 0] = "AACSequenceHeader";
        AACPacketType[AACPacketType["AACRaw"] = 1] = "AACRaw";
    })(AACPacketType || (AACPacketType = {}));

    var TagType;
    (function (TagType) {
        TagType[TagType["AUDIO"] = 8] = "AUDIO";
        TagType[TagType["VIDEO"] = 9] = "VIDEO";
        TagType[TagType["SCRIPT"] = 18] = "SCRIPT";
    })(TagType || (TagType = {}));

    /**
     * FrameType 枚举用于定义视频帧的类型
     */
    var FrameType;
    (function (FrameType) {
        /**
         * 关键帧 (Key Frame)：完整视频帧，以便解码器无需参考先前的帧。
         */
        FrameType[FrameType["KeyFrame"] = 1] = "KeyFrame";
        /**
         * 内部压缩帧 (Inter Frame)：依赖前面的关键帧或其它内部压缩帧。
         */
        FrameType[FrameType["InterFrame"] = 2] = "InterFrame";
        /**
         * 可丢弃的压缩帧 (Disposable Inter Frame)：在网络条件差时可以被丢弃。
         */
        FrameType[FrameType["DisposableInterFrame"] = 3] = "DisposableInterFrame";
        /**
         * 生成新的关键帧 (Generate Key Frame)
         */
        FrameType[FrameType["GenerateKeyFrame"] = 4] = "GenerateKeyFrame";
        /**
         * 视频信息/命令帧 (Video Info/Command Frame)：携带视频流的元数据信息。
         */
        FrameType[FrameType["VideoInfo"] = 5] = "VideoInfo";
    })(FrameType || (FrameType = {}));
    /**
     * CodeId 枚举用于定义视频编码格式类型
     */
    var CodeId;
    (function (CodeId) {
        /**
         * H.263 编码格式
         */
        CodeId[CodeId["H263"] = 2] = "H263";
        /**
         * 屏幕录制视频编码 (Screen Video)
         */
        CodeId[CodeId["ScreenVideo"] = 3] = "ScreenVideo";
        /**
         * VP6 编码格式
         */
        CodeId[CodeId["VP6"] = 4] = "VP6";
        /**
         * 带有 Alpha 通道的 VP6 编码格式
         */
        CodeId[CodeId["VP6WithAlphaChannel"] = 5] = "VP6WithAlphaChannel";
        /**
         * 屏幕录制视频2 (Screen Video 2) 编码格式
         */
        CodeId[CodeId["ScreenVideo2"] = 6] = "ScreenVideo2";
        /**
         * AVC 编码格式 (H.264)
         */
        CodeId[CodeId["AVC"] = 7] = "AVC";
    })(CodeId || (CodeId = {}));
    /**
     * 枚举表示不同类型的 AVC（高级视频编码）包。
     */
    var AvcPacketType;
    (function (AvcPacketType) {
        /**
         * SequenceHeader 表示 AVC 的序列头包类型。
         */
        AvcPacketType[AvcPacketType["SequenceHeader"] = 0] = "SequenceHeader";
        /**
         * Nalu（网络抽象层单元）表示 AVC 的 NAL 单元包类型。
         */
        AvcPacketType[AvcPacketType["NALU"] = 1] = "NALU";
        /**
         * EndOfSequence 表示 AVC 序列的结束。
         */
        AvcPacketType[AvcPacketType["EndOfSequence"] = 2] = "EndOfSequence";
    })(AvcPacketType || (AvcPacketType = {}));

    /**
     * AMF (Action Message Format) 数据类型枚举。
     */
    var AmfType;
    (function (AmfType) {
        /** 数字类型，值为 0x00 */
        AmfType[AmfType["NUMBER"] = 0] = "NUMBER";
        /** 布尔类型，值为 0x01 */
        AmfType[AmfType["BOOLEAN"] = 1] = "BOOLEAN";
        /** 字符串类型，值为 0x02 */
        AmfType[AmfType["STRING"] = 2] = "STRING";
        /** 对象类型，值为 0x03 */
        AmfType[AmfType["OBJECT"] = 3] = "OBJECT";
        /** MovieClip 类型，值为 0x04 */
        AmfType[AmfType["MOVIE_CLIP"] = 4] = "MOVIE_CLIP";
        /** Null 类型，值为 0x05 */
        AmfType[AmfType["NULL"] = 5] = "NULL";
        /** Undefined 类型，值为 0x06 */
        AmfType[AmfType["UNDEFINED"] = 6] = "UNDEFINED";
        /** 引用类型，值为 0x07 */
        AmfType[AmfType["REFERENCE"] = 7] = "REFERENCE";
        /** ECMA 数组类型，值为 0x08 */
        AmfType[AmfType["ECMA_ARRAY"] = 8] = "ECMA_ARRAY";
        /** 对象结束标记，值为 0x09 */
        AmfType[AmfType["OBJECT_END_MARKER"] = 9] = "OBJECT_END_MARKER";
        /** 严格数组类型，值为 0x0a */
        AmfType[AmfType["STRICT_ARRAY"] = 10] = "STRICT_ARRAY";
        /** 日期类型，值为 0x0b */
        AmfType[AmfType["DATE"] = 11] = "DATE";
        /** 长字符串类型，值为 0x0c */
        AmfType[AmfType["LONG_STRING"] = 12] = "LONG_STRING";
    })(AmfType || (AmfType = {}));

    var _BinaryWriter_position, _BinaryWriter_buffer, _BinaryWriter_view, _BinaryWriter_littleEndian;
    const DEFAULT_BUFFER = 1024;
    const MIN_GROWTH = 512;
    class BinaryWriter {
        constructor(littleEndian = false) {
            _BinaryWriter_position.set(this, 0);
            _BinaryWriter_buffer.set(this, void 0);
            _BinaryWriter_view.set(this, void 0);
            _BinaryWriter_littleEndian.set(this, void 0);
            __classPrivateFieldSet(this, _BinaryWriter_buffer, new Uint8Array(DEFAULT_BUFFER), "f");
            __classPrivateFieldSet(this, _BinaryWriter_view, new DataView(__classPrivateFieldGet(this, _BinaryWriter_buffer, "f").buffer), "f");
            __classPrivateFieldSet(this, _BinaryWriter_littleEndian, littleEndian, "f");
        }
        writeUint8(value) {
            var _a, _b;
            this.ensureAvailable(1);
            __classPrivateFieldGet(this, _BinaryWriter_buffer, "f")[__classPrivateFieldSet(this, _BinaryWriter_position, (_b = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _a = _b++, _b), "f"), _a] = value;
        }
        writeUint16(value) {
            this.ensureAvailable(2);
            __classPrivateFieldGet(this, _BinaryWriter_view, "f").setUint16(__classPrivateFieldGet(this, _BinaryWriter_position, "f"), value, __classPrivateFieldGet(this, _BinaryWriter_littleEndian, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + 2, "f");
        }
        writeUint24(value) {
            var _a, _b, _c, _d, _e, _f, _g, _h, _j, _k, _l, _m;
            this.ensureAvailable(3);
            if (__classPrivateFieldGet(this, _BinaryWriter_littleEndian, "f")) {
                __classPrivateFieldGet(this, _BinaryWriter_buffer, "f")[__classPrivateFieldSet(this, _BinaryWriter_position, (_b = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _a = _b++, _b), "f"), _a] = value & 0xff;
                __classPrivateFieldGet(this, _BinaryWriter_buffer, "f")[__classPrivateFieldSet(this, _BinaryWriter_position, (_d = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _c = _d++, _d), "f"), _c] = (value >> 8) & 0xff;
                __classPrivateFieldGet(this, _BinaryWriter_buffer, "f")[__classPrivateFieldSet(this, _BinaryWriter_position, (_f = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _e = _f++, _f), "f"), _e] = (value >> 16) & 0xff;
            }
            else {
                __classPrivateFieldGet(this, _BinaryWriter_buffer, "f")[__classPrivateFieldSet(this, _BinaryWriter_position, (_h = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _g = _h++, _h), "f"), _g] = (value >> 16) & 0xff;
                __classPrivateFieldGet(this, _BinaryWriter_buffer, "f")[__classPrivateFieldSet(this, _BinaryWriter_position, (_k = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _j = _k++, _k), "f"), _j] = (value >> 8) & 0xff;
                __classPrivateFieldGet(this, _BinaryWriter_buffer, "f")[__classPrivateFieldSet(this, _BinaryWriter_position, (_m = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _l = _m++, _m), "f"), _l] = value & 0xff;
            }
        }
        writeUint32(value) {
            this.ensureAvailable(4);
            __classPrivateFieldGet(this, _BinaryWriter_view, "f").setUint32(__classPrivateFieldGet(this, _BinaryWriter_position, "f"), value, __classPrivateFieldGet(this, _BinaryWriter_littleEndian, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + 4, "f");
        }
        writeInt8(value) {
            var _a, _b;
            this.ensureAvailable(1);
            __classPrivateFieldGet(this, _BinaryWriter_view, "f").setInt8((__classPrivateFieldSet(this, _BinaryWriter_position, (_b = __classPrivateFieldGet(this, _BinaryWriter_position, "f"), _a = _b++, _b), "f"), _a), value);
        }
        writeInt16(value) {
            this.ensureAvailable(2);
            __classPrivateFieldGet(this, _BinaryWriter_view, "f").setInt16(__classPrivateFieldGet(this, _BinaryWriter_position, "f"), value, __classPrivateFieldGet(this, _BinaryWriter_littleEndian, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + 2, "f");
        }
        writeInt32(value) {
            this.ensureAvailable(4);
            __classPrivateFieldGet(this, _BinaryWriter_view, "f").setInt32(__classPrivateFieldGet(this, _BinaryWriter_position, "f"), value, __classPrivateFieldGet(this, _BinaryWriter_littleEndian, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + 4, "f");
        }
        writeFloat32(value) {
            this.ensureAvailable(4);
            __classPrivateFieldGet(this, _BinaryWriter_view, "f").setFloat32(__classPrivateFieldGet(this, _BinaryWriter_position, "f"), value, __classPrivateFieldGet(this, _BinaryWriter_littleEndian, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + 4, "f");
        }
        writeFloat64(value) {
            this.ensureAvailable(8);
            __classPrivateFieldGet(this, _BinaryWriter_view, "f").setFloat64(__classPrivateFieldGet(this, _BinaryWriter_position, "f"), value, __classPrivateFieldGet(this, _BinaryWriter_littleEndian, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + 8, "f");
        }
        writeBytes(bytes) {
            this.ensureAvailable(bytes.byteLength);
            __classPrivateFieldGet(this, _BinaryWriter_buffer, "f").set(bytes, __classPrivateFieldGet(this, _BinaryWriter_position, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + bytes.byteLength, "f");
        }
        writeString(str) {
            const encoder = new TextEncoder();
            const encodedStr = encoder.encode(str);
            this.ensureAvailable(encodedStr.byteLength);
            __classPrivateFieldGet(this, _BinaryWriter_buffer, "f").set(encodedStr, __classPrivateFieldGet(this, _BinaryWriter_position, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_position, __classPrivateFieldGet(this, _BinaryWriter_position, "f") + encodedStr.byteLength, "f");
        }
        getBytes() {
            const result = new Uint8Array(__classPrivateFieldGet(this, _BinaryWriter_position, "f"));
            result.set(__classPrivateFieldGet(this, _BinaryWriter_buffer, "f").subarray(0, __classPrivateFieldGet(this, _BinaryWriter_position, "f")));
            return result;
        }
        getPosition() {
            return __classPrivateFieldGet(this, _BinaryWriter_position, "f");
        }
        reset() {
            __classPrivateFieldSet(this, _BinaryWriter_position, 0, "f");
        }
        seek(position) {
            if (position < 0 || position > __classPrivateFieldGet(this, _BinaryWriter_buffer, "f").byteLength) {
                throw new Error("非法的位置");
            }
            __classPrivateFieldSet(this, _BinaryWriter_position, position, "f");
        }
        ensureAvailable(bytes) {
            const requiredSize = bytes + __classPrivateFieldGet(this, _BinaryWriter_position, "f");
            if (requiredSize <= __classPrivateFieldGet(this, _BinaryWriter_buffer, "f").byteLength) {
                return;
            }
            // 计算新的缓冲区大小
            const newSize = Math.max(__classPrivateFieldGet(this, _BinaryWriter_buffer, "f").byteLength * 2, requiredSize + MIN_GROWTH);
            // 创建新的缓冲区
            const newBuffer = new Uint8Array(newSize);
            newBuffer.set(__classPrivateFieldGet(this, _BinaryWriter_buffer, "f"));
            __classPrivateFieldSet(this, _BinaryWriter_buffer, newBuffer, "f");
            __classPrivateFieldSet(this, _BinaryWriter_view, new DataView(__classPrivateFieldGet(this, _BinaryWriter_buffer, "f").buffer), "f");
        }
    }
    _BinaryWriter_position = new WeakMap(), _BinaryWriter_buffer = new WeakMap(), _BinaryWriter_view = new WeakMap(), _BinaryWriter_littleEndian = new WeakMap();

    class ScriptEncoder {
        constructor() {
            Object.defineProperty(this, "writer", {
                enumerable: true,
                configurable: true,
                writable: true,
                value: void 0
            });
            this.writer = new BinaryWriter();
        }
        writeScriptDataDate(date) {
            this.writer.writeFloat64(date.getTime());
            this.writer.writeUint16(0); // 时区，默认为0
        }
        writeScriptDataEcmaArray(obj) {
            this.writer.writeUint32(Object.keys(obj).length); // 数组长度
            for (const [key, value] of Object.entries(obj)) {
                this.writeScriptDataObjectProperty(key, value);
            }
            this.writeScriptDataObjectEnd();
        }
        writeScriptDataLongString(str) {
            const strBytes = new TextEncoder().encode(str);
            this.writer.writeUint32(strBytes.length);
            this.writer.writeBytes(strBytes);
        }
        writeScriptDataObject(objs) {
            for (const [key, value] of Object.entries(objs)) {
                this.writeScriptDataObjectProperty(key, value);
            }
            this.writeScriptDataObjectEnd();
        }
        writeScriptDataObjectEnd() {
            this.writer.writeUint8(0x00);
            this.writer.writeUint8(0x00);
            this.writer.writeUint8(0x09);
        }
        writeScriptDataObjectProperty(key, value) {
            this.writeScriptDataString(key);
            this.writeScriptDataValue(value);
        }
        writeScriptDataStrictArray(arr) {
            this.writer.writeUint32(arr.length);
            for (const value of arr) {
                this.writeScriptDataValue(value);
            }
        }
        writeScriptDataString(str) {
            const encoder = new TextEncoder();
            const strBytes = encoder.encode(str);
            this.writer.writeUint16(strBytes.length);
            this.writer.writeBytes(strBytes);
        }
        writeScriptDataValue(value) {
            if (value === null) {
                this.writer.writeUint8(AmfType.NULL);
            }
            else if (value === undefined) {
                this.writer.writeUint8(AmfType.UNDEFINED);
            }
            else if (typeof value === "boolean") {
                this.writer.writeUint8(AmfType.BOOLEAN);
                this.writer.writeUint8(value ? 0x01 : 0x00);
            }
            else if (typeof value === "number") {
                this.writer.writeUint8(AmfType.NUMBER);
                this.writer.writeFloat64(value);
            }
            else if (typeof value === "string") {
                if (value.length > 65535) {
                    this.writer.writeUint8(AmfType.LONG_STRING);
                    this.writeScriptDataLongString(value);
                }
                else {
                    this.writer.writeUint8(AmfType.STRING);
                    this.writeScriptDataString(value);
                }
            }
            else if (value instanceof Date) {
                this.writer.writeUint8(AmfType.DATE);
                this.writeScriptDataDate(value);
            }
            else if (Array.isArray(value)) {
                this.writer.writeUint8(AmfType.STRICT_ARRAY);
                this.writeScriptDataStrictArray(value);
            }
            else if (typeof value === "object") {
                this.writer.writeUint8(AmfType.ECMA_ARRAY);
                this.writeScriptDataEcmaArray(value);
            }
        }
    }

    class FlvEncoder extends ScriptEncoder {
        constructor() {
            super();
        }
        encodeFlvHeader(hasVideo = true, hasAudio = true) {
            // 重置写入器
            this.writer.reset();
            // FLV 签名，分别为 "F"、"L"、"V" 的 ASCII 值
            this.writer.writeString("FLV");
            // 设置 FLV 文件版本为 1
            this.writer.writeUint8(1);
            // 流标志位 (5位保留 + 1位音频 + 1位保留 + 1位视频)
            const flag = (hasAudio ? 0x04 : 0) | (hasVideo ? 0x01 : 0);
            this.writer.writeUint8(flag);
            // 数据部分长度，这里固定为 9 字节
            this.writer.writeUint32(9);
            // 结束标志前4个字节是前一个 Tag 的大小
            this.writer.writeUint32(0);
            return this.writer.getBytes();
        }
        encodeFlvTag(type, header, timestamp, data) {
            const dataSize = header.byteLength + data.byteLength;
            this.writer.reset();
            this.writer.writeUint8(TagType[type]);
            // 设置 DataSize(tag大小 - 11)
            this.writer.writeUint24(dataSize);
            // 设置时间戳
            this.writer.writeUint24(timestamp & 0xffffff);
            this.writer.writeUint8((timestamp >> 24) & 0xff); // 拓展位
            // 设置 StreamID，总是0
            this.writer.writeUint24(0);
            // 写入标签头部
            this.writer.writeBytes(header);
            // 写入标签数据
            this.writer.writeBytes(data);
            // 写入前一个标签大小(4字节)
            // 标签大小 = 标签头(11) + 数据大小(dataSize)
            this.writer.writeUint32(11 + dataSize);
            return this.writer.getBytes();
        }
        encodeScriptDataTag(metadata) {
            this.writer.reset();
            this.writeScriptDataValue("onMetaData");
            this.writeScriptDataValue(metadata);
            // 创建 ScriptTag
            const scriptTag = this.encodeFlvTag("SCRIPT", new Uint8Array(0), 0, this.writer.getBytes());
            return scriptTag;
        }
        encodeAudioTag(params) {
            const { soundFormat, soundRate, soundSize, soundType, timestamp, audioData, } = params;
            // 音频格式（4位） | 采样率（2位） | 音频样本大小（1位） | 音频类型（1位）
            const firstByte = (SoundFormat[soundFormat] << 4) |
                (SoundRate[soundRate] << 2) |
                (SoundSize[soundSize] << 1) |
                SoundType[soundType];
            if (soundFormat === "AAC" && audioData) {
                const header = new Uint8Array([
                    firstByte,
                    AACPacketType[params.aacPacketType],
                ]);
                return this.encodeFlvTag("AUDIO", header, timestamp, audioData);
            }
        }
        encodeVideoTag(frameType, codecId, avcPacketType, compositionTime, timestamp, videoBody) {
            const header = new Uint8Array(5);
            header[0] = (FrameType[frameType] << 4) | CodeId[codecId];
            header[1] = AvcPacketType[avcPacketType];
            header[2] = (compositionTime >> 16) & 0xff;
            header[3] = (compositionTime >> 8) & 0xff;
            header[4] = compositionTime & 0xff;
            return this.encodeFlvTag("VIDEO", header, timestamp, videoBody);
        }
    }

    var _FlvMuxer_instances, _FlvMuxer_encoder, _FlvMuxer_eventBus, _FlvMuxer_streamProcessor, _FlvMuxer_outputStream, _FlvMuxer_options, _FlvMuxer_sourceStream, _FlvMuxer_sourceStreamController, _FlvMuxer_muxStream, _FlvMuxer_strategies, _FlvMuxer_readableHandler, _FlvMuxer_initStrategies, _FlvMuxer_initSourceStream, _FlvMuxer_initMuxStream, _FlvMuxer_encodeMetadata, _FlvMuxer_muxChunk;
    class FlvMuxer {
        get state() {
            return __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").state;
        }
        constructor(writable, options) {
            _FlvMuxer_instances.add(this);
            _FlvMuxer_encoder.set(this, void 0);
            _FlvMuxer_eventBus.set(this, void 0);
            _FlvMuxer_streamProcessor.set(this, void 0);
            _FlvMuxer_outputStream.set(this, void 0);
            _FlvMuxer_options.set(this, void 0);
            _FlvMuxer_sourceStream.set(this, void 0);
            _FlvMuxer_sourceStreamController.set(this, void 0);
            _FlvMuxer_muxStream.set(this, void 0);
            _FlvMuxer_strategies.set(this, {});
            _FlvMuxer_readableHandler.set(this, void 0);
            if (!(writable instanceof WritableStream)) {
                throw new Error("The provided 'writable' is not an instance of WritableStream.");
            }
            __classPrivateFieldSet(this, _FlvMuxer_encoder, new FlvEncoder(), "f");
            __classPrivateFieldSet(this, _FlvMuxer_eventBus, EventBus.getInstance(), "f");
            __classPrivateFieldSet(this, _FlvMuxer_streamProcessor, StreamProcessor.getInstance(), "f");
            __classPrivateFieldSet(this, _FlvMuxer_outputStream, writable, "f");
            __classPrivateFieldSet(this, _FlvMuxer_options, options, "f");
            // 初始化策略
            __classPrivateFieldGet(this, _FlvMuxer_instances, "m", _FlvMuxer_initStrategies).call(this);
        }
        addRawChunk(type, chunk) {
            __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").addTrackChunk(type, chunk);
        }
        configureAudio(options) {
            if (!options.encoderConfig) {
                throw new Error("Audio encoder configuration cannot be empty");
            }
            __classPrivateFieldGet(this, _FlvMuxer_options, "f").audio = options;
            __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").addAudioTrack(new AudioEncoderTrack(options.encoderConfig));
        }
        configureVideo(options) {
            if (!options.encoderConfig) {
                throw new Error("Video encoder configuration cannot be empty");
            }
            __classPrivateFieldGet(this, _FlvMuxer_options, "f").video = options;
            __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").addVideoTrack(new VideoEncoderTrack(options.encoderConfig, options.keyframeInterval));
        }
        async start() {
            if (!__classPrivateFieldGet(this, _FlvMuxer_options, "f")?.audio && !__classPrivateFieldGet(this, _FlvMuxer_options, "f")?.video) {
                throw new Error("Muxer is not configured with audio or video tracks. Please call configureAudio() or configureVideo() first.");
            }
            try {
                __classPrivateFieldGet(this, _FlvMuxer_instances, "m", _FlvMuxer_initSourceStream).call(this);
                __classPrivateFieldGet(this, _FlvMuxer_instances, "m", _FlvMuxer_initMuxStream).call(this);
                if (!__classPrivateFieldGet(this, _FlvMuxer_sourceStream, "f") || !__classPrivateFieldGet(this, _FlvMuxer_muxStream, "f") || !__classPrivateFieldGet(this, _FlvMuxer_outputStream, "f")) {
                    throw new Error("Failed to initialize streams");
                }
                __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").start();
                return __classPrivateFieldGet(this, _FlvMuxer_sourceStream, "f")
                    .pipeThrough(__classPrivateFieldGet(this, _FlvMuxer_muxStream, "f"))
                    .pipeTo(__classPrivateFieldGet(this, _FlvMuxer_outputStream, "f"), {
                    preventClose: true,
                });
            }
            catch (error) {
                throw new Error(`Error starting Muxer: ${error}`);
            }
        }
        pause() {
            return __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").pause();
        }
        resume() {
            __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").resume();
        }
        stop() {
            if (__classPrivateFieldGet(this, _FlvMuxer_readableHandler, "f")) {
                __classPrivateFieldGet(this, _FlvMuxer_eventBus, "f").off("CHUNK_PUBLISH", __classPrivateFieldGet(this, _FlvMuxer_readableHandler, "f"));
            }
            __classPrivateFieldGet(this, _FlvMuxer_sourceStreamController, "f")?.close();
            return __classPrivateFieldGet(this, _FlvMuxer_streamProcessor, "f").stop();
        }
    }
    _FlvMuxer_encoder = new WeakMap(), _FlvMuxer_eventBus = new WeakMap(), _FlvMuxer_streamProcessor = new WeakMap(), _FlvMuxer_outputStream = new WeakMap(), _FlvMuxer_options = new WeakMap(), _FlvMuxer_sourceStream = new WeakMap(), _FlvMuxer_sourceStreamController = new WeakMap(), _FlvMuxer_muxStream = new WeakMap(), _FlvMuxer_strategies = new WeakMap(), _FlvMuxer_readableHandler = new WeakMap(), _FlvMuxer_instances = new WeakSet(), _FlvMuxer_initStrategies = function _FlvMuxer_initStrategies() {
        __classPrivateFieldGet(this, _FlvMuxer_strategies, "f")["AAC_SE"] = new AACSEStrategy();
        __classPrivateFieldGet(this, _FlvMuxer_strategies, "f")["AVC_SE"] = new AVCSEStrategy();
        __classPrivateFieldGet(this, _FlvMuxer_strategies, "f")["AAC_RAW"] = new AACRawStrategy();
        __classPrivateFieldGet(this, _FlvMuxer_strategies, "f")["AVC_NALU"] = new AVCNALUStrategy();
    }, _FlvMuxer_initSourceStream = function _FlvMuxer_initSourceStream() {
        __classPrivateFieldSet(this, _FlvMuxer_sourceStream, new ReadableStream({
            start: (controller) => {
                __classPrivateFieldSet(this, _FlvMuxer_sourceStreamController, controller, "f");
                __classPrivateFieldSet(this, _FlvMuxer_readableHandler, (chunk) => {
                    controller.enqueue(chunk);
                }, "f");
                __classPrivateFieldGet(this, _FlvMuxer_eventBus, "f").on("CHUNK_PUBLISH", __classPrivateFieldGet(this, _FlvMuxer_readableHandler, "f"));
            },
        }), "f");
    }, _FlvMuxer_initMuxStream = function _FlvMuxer_initMuxStream() {
        __classPrivateFieldSet(this, _FlvMuxer_muxStream, new TransformStream({
            start: async (controller) => {
                const header = __classPrivateFieldGet(this, _FlvMuxer_encoder, "f").encodeFlvHeader(!!__classPrivateFieldGet(this, _FlvMuxer_options, "f").video, !!__classPrivateFieldGet(this, _FlvMuxer_options, "f").audio);
                const metadata = __classPrivateFieldGet(this, _FlvMuxer_instances, "m", _FlvMuxer_encodeMetadata).call(this);
                controller.enqueue(header);
                controller.enqueue(metadata);
            },
            transform: (chunk, controller) => {
                const tag = __classPrivateFieldGet(this, _FlvMuxer_instances, "m", _FlvMuxer_muxChunk).call(this, chunk);
                controller.enqueue(tag);
            },
        }), "f");
    }, _FlvMuxer_encodeMetadata = function _FlvMuxer_encodeMetadata() {
        const metadata = {
            duration: 0,
            encoder: "flv-muxer.js",
        };
        if (__classPrivateFieldGet(this, _FlvMuxer_options, "f").video) {
            const { encoderConfig } = __classPrivateFieldGet(this, _FlvMuxer_options, "f").video;
            Object.assign(metadata, {
                videocodecid: 7,
                width: encoderConfig.width,
                height: encoderConfig.height,
                framerate: encoderConfig.framerate,
            });
        }
        if (__classPrivateFieldGet(this, _FlvMuxer_options, "f").audio) {
            const { encoderConfig } = __classPrivateFieldGet(this, _FlvMuxer_options, "f").audio;
            Object.assign(metadata, {
                audiocodecid: 10,
                audiodatarate: (encoderConfig.bitrate ?? 0) / 1000,
                stereo: encoderConfig.numberOfChannels === 2,
                audiosamplerate: encoderConfig.sampleRate,
            });
        }
        const scriptData = __classPrivateFieldGet(this, _FlvMuxer_encoder, "f").encodeScriptDataTag(metadata);
        return scriptData;
    }, _FlvMuxer_muxChunk = function _FlvMuxer_muxChunk(chunk) {
        if (!chunk)
            return;
        const strategy = __classPrivateFieldGet(this, _FlvMuxer_strategies, "f")[chunk.type];
        if (strategy) {
            return strategy.process(chunk, __classPrivateFieldGet(this, _FlvMuxer_encoder, "f"));
        }
    };

    return FlvMuxer;

})();
//# sourceMappingURL=flv-muxer.iife.js.map
