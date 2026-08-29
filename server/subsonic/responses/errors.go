package responses

// Subsonic 协议定义的错误码。
const (
	ErrorGeneric            int32 = 0
	ErrorMissingParameter   int32 = 10
	ErrorClientTooOld       int32 = 20
	ErrorServerTooOld       int32 = 30
	ErrorAuthenticationFail int32 = 40
	ErrorAuthorizationFail  int32 = 50
	ErrorTrialExpired       int32 = 60
	ErrorDataNotFound       int32 = 70
)

// errors 是错误码对应的标准提示文案。
var errors = map[int32]string{
	ErrorGeneric:            "A generic error",
	ErrorMissingParameter:   "Required parameter is missing",
	ErrorClientTooOld:       "Incompatible Subsonic REST protocol version. Client must upgrade",
	ErrorServerTooOld:       "Incompatible Subsonic REST protocol version. Server must upgrade",
	ErrorAuthenticationFail: "Wrong username or password",
	ErrorAuthorizationFail:  "User is not authorized for the given operation",
	ErrorTrialExpired:       "The trial period for the Subsonic server is over. Please upgrade to Subsonic Premium. Visit subsonic.org for details",
	ErrorDataNotFound:       "The requested data was not found",
}

// ErrorMsg 返回错误码对应的提示，未知码退回通用错误。
func ErrorMsg(code int32) string {
	if v, found := errors[code]; found {
		return v
	}
	return errors[ErrorGeneric]
}
