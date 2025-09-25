# 不规范设备耻辱柱

## 宇视

### IPC2A3L-FW-APF60-V1-DT

sdp 中缺失必填字段 (正确格式c=IN IP4 0.0.0.0)

> https://tools.ietf.org/html/rfc4566#section-8.2.6

```sdp
v=0
o=- 1001 1 IN 
s=VCP IPC Realtime stream
m=video 0 RTP/AVP 105
c=IN 
a=control:rtsp://192.168.0.101/media/video1/video
a=rtpmap:105 H264/90000
a=fmtp:105 profile-level-id=640032; packetization-mode=1; sprop-parameter-sets=Z2QAMqw7UBIAUdCAAAH0AABhqEI=,aO48sA==
a=recvonly
m=audio 0 RTP/AVP 0
c=IN 
a=fmtp:0 RTCP=0
a=control:rtsp://192.168.0.101/media/video1/audio1
a=recvonly
m=application 0 RTP/AVP 107
c=IN 
a=control:rtsp://192.168.0.101/media/video1/metadata
a=rtpmap:107 vnd.onvif.metadata/90000
a=fmtp:107 DecoderTag=h3c-v3 RTCP=0
a=recvonly
```