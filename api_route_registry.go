package m7s

import "sync"

type APIRouteUnaryFactory func() any

var apiRouteUnaryFactories sync.Map // map[string]APIRouteUnaryFactory

// apiRouteBroadcastOnNotFound marks methods that must reach every node when the
// stream cannot be resolved anywhere in the cluster. Record metadata and
// auto-resume intents are node-local, so once a stream is offline a
// stream-addressed route can no longer reach the node holding them.
var apiRouteBroadcastOnNotFound sync.Map // map[string]struct{}

// RegisterAPIRouteUnary registers a unary gRPC method that is safe to route across nodes,
// along with a response factory so the router can unmarshal the reply.
func RegisterAPIRouteUnary(fullMethod string, factory APIRouteUnaryFactory) {
	if fullMethod == "" || factory == nil {
		return
	}
	apiRouteUnaryFactories.Store(fullMethod, factory)
}

func apiRouteUnaryFactory(fullMethod string) (APIRouteUnaryFactory, bool) {
	if v, ok := apiRouteUnaryFactories.Load(fullMethod); ok {
		return v.(APIRouteUnaryFactory), true
	}
	return nil, false
}

// RegisterAPIRouteBroadcastOnNotFound declares that a routed method must also be
// delivered to all peers when the stream is not found in the cluster.
// The handler must be safe to invoke when the stream is absent (it is still run
// locally, and on every peer, even if no node owns the stream).
func RegisterAPIRouteBroadcastOnNotFound(fullMethod string) {
	if fullMethod == "" {
		return
	}
	apiRouteBroadcastOnNotFound.Store(fullMethod, struct{}{})
}

func apiRouteShouldBroadcastOnNotFound(fullMethod string) bool {
	_, ok := apiRouteBroadcastOnNotFound.Load(fullMethod)
	return ok
}
