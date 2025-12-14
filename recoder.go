package m7s

import (
	"fmt"
	"path/filepath"
	"time"

	"gorm.io/gorm"

	task "github.com/langhuihui/gotask"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

type (
	IRecorder interface {
		task.ITask
		GetRecordJob() *RecordJob
	}
	RecorderFactory = func(config.Record) IRecorder
	// RecordEvent 包含录像事件的公共字段

	EventRecordStream struct {
		*config.RecordEvent
		RecordStream
	}
	RecordJob struct {
		task.Job
		Event         *config.RecordEvent
		StreamPath    string // 对应本地流
		Plugin        *Plugin
		Subscriber    *Subscriber
		SubConf       *config.Subscribe
		RecConf       *config.Record
		recorder      IRecorder
		storage       storage.Storage // 本地存储实例（录制时使用）
		TargetStorage map[string]any  // 目标存储配置（上传时使用）
		DeleteLocal   bool            // 上传成功后是否删除本地文件
	}
	DefaultRecorder struct {
		task.Task
		RecordJob RecordJob
		Event     EventRecordStream
	}
	RecordStream struct {
		ID           uint      `gorm:"primarykey"`
		StartTime    time.Time `gorm:"default:NULL"`
		EndTime      time.Time `gorm:"default:NULL"`
		Duration     uint32    `gorm:"comment:录像时长;default:0"`
		Filename     string    `json:"fileName" desc:"文件名" gorm:"type:varchar(255);comment:文件名"`
		Type         string    `json:"type" desc:"录像文件类型" gorm:"type:varchar(255);comment:录像文件类型,flv,mp4,raw,fmp4,hls"`
		FilePath     string
		StreamPath   string
		AudioCodec   string
		VideoCodec   string
		CreatedAt    time.Time
		DeletedAt    gorm.DeletedAt    `gorm:"index" yaml:"-"`
		RecordLevel  config.EventLevel `json:"eventLevel" desc:"事件级别" gorm:"type:varchar(255);comment:事件级别,high表示重要事件，无法删除且表示无需自动删除,low表示非重要事件,达到自动删除时间后，自动删除;default:'low'"`
		StorageLevel int               `json:"storageLevel" desc:"存储级别" gorm:"comment:存储级别,1=主存储,2=次级存储;default:1"`
		StorageType  string            `json:"storageType" desc:"存储类型" gorm:"type:varchar(20);comment:存储类型(local/s3/oss/cos);default:'local'"`
		// 上传状态相关字段
		UploadStatus string    `json:"uploadStatus" gorm:"type:varchar(20);comment:上传状态(pending/uploading/completed/failed);default:'pending'"`
		UploadError  string    `json:"uploadError" gorm:"type:text;comment:上传错误信息"`
		RetryCount   int       `json:"retryCount" gorm:"comment:重试次数;default:0"`
		LastRetryAt  time.Time `json:"lastRetryAt" gorm:"comment:最后重试时间"`
		FileSize     int64     `json:"fileSize" gorm:"comment:文件大小(字节)"`
		RemotePath   string    `json:"remotePath" gorm:"type:varchar(512);comment:远程存储路径"`
	}
)

func (r *DefaultRecorder) GetRecordJob() *RecordJob {
	return &r.RecordJob
}

func (r *DefaultRecorder) Start() (err error) {
	return r.RecordJob.Subscribe()
}

func (r *DefaultRecorder) CreateStream(start time.Time, customFileName func(*RecordJob) string) (err error) {
	recordJob := &r.RecordJob
	sub := recordJob.Subscriber

	// 生成文件路径
	filePath := customFileName(recordJob)
	fileName := filepath.Base(filePath)

	// 记录存储配置日志
	r.Info("CreateStream: using local storage for recording", "streamPath", recordJob.StreamPath, "filePath", filePath)

	// 始终使用本地存储进行录制
	recordJob.storage = r.createLocalStorage()
	if recordJob.storage == nil {
		return fmt.Errorf("failed to create local storage")
	}

	// 保存目标存储配置（用于上传）
	recordJob.TargetStorage = recordJob.RecConf.Storage
	recordJob.DeleteLocal = true // 默认上传成功后删除本地文件

	// 确定最终的存储类型（用于数据库记录）
	var targetStorageType = "local"
	for t := range recordJob.RecConf.Storage {
		if t != "local" {
			targetStorageType = t
			break
		}
	}

	// 如果配置了云存储，初始状态为 pending
	uploadStatus := storage.UploadStatusCompleted // 本地存储默认就是完成
	if targetStorageType != "local" {
		uploadStatus = storage.UploadStatusPending // 需要上传到云存储
	}

	r.Event.RecordStream = RecordStream{
		StartTime:    start,
		StreamPath:   sub.StreamPath,
		FilePath:     filePath,
		Filename:     fileName,
		Type:         recordJob.RecConf.Type,
		StorageLevel: 1,
		StorageType:  "local", // 录制时始终是本地
		UploadStatus: uploadStatus,
		RemotePath:   filePath, // 远程路径默认与本地相同
	}

	if sub.Publisher.HasAudioTrack() {
		r.Event.AudioCodec = sub.Publisher.AudioTrack.ICodecCtx.String()
	}
	if sub.Publisher.HasVideoTrack() {
		r.Event.VideoCodec = sub.Publisher.VideoTrack.ICodecCtx.String()
	}

	if recordJob.Plugin.DB != nil && recordJob.RecConf.Mode != config.RecordModeTest {
		if recordJob.Event != nil {
			r.Event.RecordEvent = recordJob.Event
			r.Event.RecordLevel = recordJob.Event.EventLevel
			recordJob.Plugin.DB.Save(&r.Event.RecordStream)
			recordJob.Plugin.DB.Save(&r.Event)
		} else {
			recordJob.Plugin.DB.Save(&r.Event.RecordStream)
		}
	}
	return
}

// createLocalStorage 创建本地存储实例（录制时始终使用本地存储）
func (r *DefaultRecorder) createLocalStorage() storage.Storage {
	localStorage, err := storage.CreateStorage("local", r.RecordJob.RecConf.FilePath)
	if err != nil {
		r.Error("create local storage failed", "err", err)
		return nil
	}
	r.Info("local storage created", "path", r.RecordJob.RecConf.FilePath)
	return localStorage
}

// GetTargetStorage 获取目标存储配置（用于上传）
func (p *RecordJob) GetTargetStorage() map[string]any {
	return p.TargetStorage
}

// ShouldUpload 检查是否需要上传到云存储
func (p *RecordJob) ShouldUpload() bool {
	if p.TargetStorage == nil {
		return false
	}
	for t := range p.TargetStorage {
		if t != "local" {
			return true
		}
	}
	return false
}

// ShouldDeleteLocal 检查上传成功后是否删除本地文件
func (p *RecordJob) ShouldDeleteLocal() bool {
	return p.DeleteLocal
}

func (r *DefaultRecorder) WriteTail(end time.Time, tailJob task.IJob) {
	r.Event.EndTime = end
	if r.RecordJob.Plugin.DB != nil && r.RecordJob.RecConf.Mode != config.RecordModeTest {
		// 将事件和录像记录关联
		if r.RecordJob.Event != nil {
			r.RecordJob.Plugin.DB.Save(&r.Event)
			r.RecordJob.Plugin.DB.Save(&r.Event.RecordStream)
		} else {
			r.RecordJob.Plugin.DB.Save(&r.Event.RecordStream)
		}
		if tailJob == nil {
			return
		}
		tailJob.AddTask(NewEventRecordCheck(r.Event.Type, r.Event.StreamPath, r.RecordJob.Plugin.DB))
	}
}

func (p *RecordJob) GetKey() string {
	return p.RecConf.FilePath
}

// GetStorage 获取存储实例
func (p *RecordJob) GetStorage() storage.Storage {
	return p.storage
}

func (p *RecordJob) Subscribe() (err error) {

	p.Subscriber, err = p.Plugin.SubscribeWithConfig(p.recorder.GetTask().Context, p.StreamPath, *p.SubConf)
	return
}

func (p *RecordJob) Init(recorder IRecorder, plugin *Plugin, streamPath string, conf config.Record, subConf *config.Subscribe) *RecordJob {
	p.Plugin = plugin
	p.RecConf = &conf
	p.Event = conf.Event
	p.StreamPath = streamPath
	if subConf == nil {
		conf := p.Plugin.config.Subscribe
		subConf = &conf
	}
	subConf.SubType = SubscribeTypeVod
	p.SubConf = subConf
	p.recorder = recorder
	p.SetDescriptions(task.Description{
		"plugin":     plugin.Meta.Name,
		"streamPath": streamPath,
		"filePath":   conf.FilePath,
		"append":     conf.Append,
		"fragment":   conf.Fragment,
	})
	recorder.SetRetry(-1, time.Second)
	if sender, webhook := plugin.getHookSender(config.HookOnRecordStart); sender != nil {
		recorder.OnStart(func() {
			alarmInfo := AlarmInfo{
				AlarmName:  string(config.HookOnRecordStart),
				AlarmType:  config.AlarmStorageExceptionRecover,
				StreamPath: streamPath,
				FilePath:   conf.FilePath,
			}
			sender(webhook, alarmInfo)
		})
	}

	if sender, webhook := plugin.getHookSender(config.HookOnRecordEnd); sender != nil {
		recorder.OnDispose(func() {
			alarmInfo := AlarmInfo{
				AlarmType:  config.AlarmStorageException,
				AlarmDesc:  recorder.StopReason().Error(),
				AlarmName:  string(config.HookOnRecordEnd),
				StreamPath: streamPath,
				FilePath:   conf.FilePath,
			}
			sender(webhook, alarmInfo)
		})
	}

	plugin.Server.Records.AddTask(p, plugin.Logger.With("filePath", conf.FilePath, "streamPath", streamPath))
	return p
}

func (p *RecordJob) Start() (err error) {
	// dir := p.FilePath
	// if p.Fragment == 0 || p.Append {
	// 	dir = filepath.Dir(p.FilePath)
	// }
	// p.SetDescription("filePath", p.FilePath)
	// if err = os.MkdirAll(dir, 0755); err != nil {
	// 	return
	// }
	p.AddTask(p.recorder, p.Logger)
	return
}

func NewEventRecordCheck(t string, streamPath string, db *gorm.DB) *eventRecordCheck {
	return &eventRecordCheck{
		DB:         db,
		streamPath: streamPath,
		Type:       t,
	}
}

type eventRecordCheck struct {
	task.Task
	DB         *gorm.DB
	streamPath string
	Type       string
}

func (t *eventRecordCheck) Run() (err error) {
	var eventRecordStreams []EventRecordStream
	queryRecord := EventRecordStream{
		RecordEvent: &config.RecordEvent{
			EventLevel: config.EventLevelHigh,
		},
		RecordStream: RecordStream{
			StreamPath: t.streamPath,
			Type:       t.Type,
		},
	}
	t.DB.Where(&queryRecord).Find(&eventRecordStreams) //搜索事件录像，且为重要事件（无法自动删除）
	if len(eventRecordStreams) > 0 {
		for _, recordStream := range eventRecordStreams {
			var unimportantEventRecordStreams []RecordStream
			query := `start_time <= ? and end_time >= ? and stream_path=? and type=?`
			t.DB.Where(query, recordStream.EndTime, recordStream.StartTime, t.streamPath, t.Type).Find(&unimportantEventRecordStreams)
			if len(unimportantEventRecordStreams) > 0 {
				for _, unimportantEventRecordStream := range unimportantEventRecordStreams {
					unimportantEventRecordStream.RecordLevel = config.EventLevelHigh
					t.DB.Save(&unimportantEventRecordStream)
				}
			}
		}
	}
	return
}
