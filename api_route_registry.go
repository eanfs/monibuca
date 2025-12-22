package m7s

import "sync"

type APIRouteUnaryFactory func() any

var apiRouteUnaryFactories sync.Map // map[string]APIRouteUnaryFactory

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
