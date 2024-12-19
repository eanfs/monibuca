package pkg

import "errors"

var (
	ErrNotFound                 = errors.New("not found")
	ErrStreamExist              = errors.New("stream exist")
	ErrRecordExists             = errors.New("record exists")
	ErrKick                     = errors.New("kick")
	ErrDiscard                  = errors.New("discard")
	ErrPublishTimeout           = errors.New("publish timeout")
	ErrPublishIdleTimeout       = errors.New("publish idle timeout")
	ErrPublishDelayCloseTimeout = errors.New("publish delay close timeout")
	ErrPushRemoteURLExist       = errors.New("push remote url exist")
	ErrSubscribeTimeout         = errors.New("subscribe timeout")
	ErrRestart                  = errors.New("restart")
	ErrInterrupt                = errors.New("interrupt")
	ErrUnsupportCodec           = errors.New("unsupport codec")
	ErrMuted                    = errors.New("muted")
	ErrNoTrack                  = errors.New("no track")
	ErrNoDB                     = errors.New("no db")
	ErrLost                     = errors.New("lost")
	ErrSeek                     = errors.New("seek")
	ErrRecordSamePath           = errors.New("record same path")
	ErrTransformSame            = errors.New("transform same")
	ErrNotListen                = errors.New("not listen")
)
