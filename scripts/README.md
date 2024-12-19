# use protoc.sh to generate the go code from proto files

1. For global proto file:
```bash
sh scripts/protoc.sh
```

2. For plugin proto file:
```bash
sh scripts/protoc.sh plugin_name
```

# use loop.py to loop the ffmpeg command

1. python scripts/loop.py

# use mock.py to mock the tcp server

使用方法:
1. 作为服务器运行 (监听端口 8554 并发送 peer 1 的数据):
```bash
python scripts/mock.py dump.rtsp 1 -l 8554
```

2. 作为客户端运行 (连接到 192.168.1.100:554 并发送 peer 0 的数据):
```bash
python scripts/mock.py dump.rtsp 0 -c 192.168.1.100:554
```
