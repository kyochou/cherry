package cherryCode

const (
	OK                    int32 = 0  // is ok
	SessionUIDNotBind     int32 = 10 // session uid not bind
	DiscoveryNotFoundNode int32 = 11 // discovery not fond node id
	AppIsStop             int32 = 12 // application is stopped
	RPCNetError           int32 = 20 // rpc net error
	RPCUnmarshalError     int32 = 21 // rpc data unmarshal error
	RPCMarshalError       int32 = 22 // rpc data marshal error
	RPCRemoteExecuteError int32 = 23 // rpc remote method executor error
	RPCReplyParamsError   int32 = 24 // rpc reply parameter error
	RPCRouteDecodeError   int32 = 25 // rpc route decode error
	RPCRouteHashError     int32 = 26 // rpc route hash error
	RPCNotImplement       int32 = 27 // rpc method not implement
	RPCHandlerError       int32 = 28 // rpc get handler error
)

func IsOK(code int32) bool {
	return code == OK
}

func IsFail(code int32) bool {
	return code != OK
}
