# 样例说明
本目录中包含了若干功能的配置样例，其中 default 目录工程启动单个服务，可以通过启动多次并配置不同的配置文件进行联调。multiple 目录可以在单个进程中同时运行两个服务，需要传入两个配置文件，具体使用方法看下面。

## 8080 和 8081 目录
- 为了防止端口冲突，8080 目录中的配置文件将启动默认端口
- 8081 目录中的配置文件将启动 8081 端口，其他协议也将开启和默认端口不通的端口防止冲突

# 启动单个服务
这里举若干例子，其他功能启动方式类似
## 启动默认服务
```bash
cd default
go run -tags sqlite main.go
```
## 录制功能
```bash
cd default
go run -tags sqlite main.go -c ../8080/record.yaml
```
推流到这个 localhost 观察录制情况

## 拉取本地 flv 文件

> 拉取 mp4 同理

修改 pull_flv_file.yaml 中的 flv 路径为本地可用的 flv 文件路径
```bash
cd default
go run -tags sqlite main.go -c ../8080/pull_flv_file.yaml
```
此时可以用 ffplay 播放
```bash
ffplay http://localhost:8080/flv/live/test
```
或者
```bash
ffplay rtmp://localhost/live/test
```
或者其他协议

## 拉取 rtmp 流
修改 pull_rtmp.yaml 中的 rtmp 路径为可用的远端 rtmp 地址
```bash
cd default
go run -tags sqlite main.go -c ../8080/pull_rtmp.yaml
```
> 拉取rtsp同理

## 转推

- 本例子需要运行两个进程，A 和 B
- A 从 flv 文件产生一个流 live/test
- B 从 A 的 live/test 流拉取，然后推送到A的 live/test2

先启动一个服务拉取 flv 文件产生一个流

修改 pull_flv_file.yaml 中的 flv 路径为本地可用的 flv 文件路径
```bash
cd default
go run -tags sqlite main.go -c ../8080/pull_flv_file.yaml
```
然后启动另一个服务将这个流推送到另一个 rtmp 地址
```bash
cd default
go run -tags sqlite main.go -c ../8081/pull_rtmp_push.yaml
```
观察第一个服务中，产生了从第二个服务推过来的 live/test2 流

## 级联

- 本例子需要运行两个进程，A 和 B
- A 启动级联插件 server 端
- B 启动级联插件 client 端

```bash
cd default
go run -tags sqlite main.go -c ../8080/cascade_server.yaml
```

```bash
cd default
go run -tags sqlite main.go -c ../8081/cascade_client.yaml
```

# 启动单进程多服务
组合方式可以参考上面的例子中包含两个进程的例子，将两个配置文件传入即可，例如：
```bash
cd multiple
go run -tags sqlite main.go -c1 ../8081/pull_rtmp_push.yaml -c2 ../8080/pull_flv_file.yaml
```
