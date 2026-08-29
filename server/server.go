package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/core/metrics"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/server/events"
	"github.com/navidrome/navidrome/ui"
)

// Server 是 Navidrome 的 HTTP 服务器，负责装配路由与中间件栈。
type Server struct {
	router   chi.Router
	ds       model.DataStore
	appRoot  string
	broker   events.Broker
	insights metrics.Insights
}

// New 创建服务器：完成首次初始化、认证初始化与路由挂载，
// 并对 FFmpeg 与外部服务凭据做一次可用性检查（仅告警，不阻断启动）。
func New(ds model.DataStore, broker events.Broker, insights metrics.Insights) *Server {
	s := &Server{ds: ds, broker: broker, insights: insights}
	initialSetup(ds)
	auth.Init(s.ds)
	s.initRoutes()
	s.mountAuthenticationRoutes()
	s.mountRootRedirector()
	checkFFmpegInstallation()
	checkExternalCredentials()
	return s
}

// MountRouter 把子路由挂到 BasePath 之下，供各 API 模块注册。
func (s *Server) MountRouter(description, urlPath string, subRouter http.Handler) {
	urlPath = path.Join(conf.Server.BasePath, urlPath)
	log.Info(fmt.Sprintf("Mounting %s routes", description), "path", urlPath)
	s.router.Group(func(r chi.Router) {
		r.Mount(urlPath, subRouter)
	})
}

// Run starts the server with the given address, and if specified, with TLS enabled.
//
// Run 启动服务并阻塞至上下文取消。
//
// 启动后等待 50ms 观察 errC：绑定失败等错误会在这段时间内暴露，
// 借此把「启动失败」与「运行中出错」区分开，避免误报「服务已就绪」。
// 退出时关闭 keep-alive 并给 3 秒优雅关闭窗口。
func (s *Server) Run(ctx context.Context, addr string, port int, tlsCert string, tlsKey string) error {
	// Mount the router for the frontend assets
	s.MountRouter("WebUI", consts.URLPathUI, s.frontendAssetsHandler())

	// Create a new http.Server with the specified read header timeout and handler
	server := &http.Server{
		ReadHeaderTimeout: consts.ServerReadHeaderTimeout,
		Handler:           s.router,
	}

	// Determine if TLS is enabled
	tlsEnabled := tlsCert != "" && tlsKey != ""

	// Validate TLS certificates before starting the server
	if tlsEnabled {
		if err := validateTLSCertificates(tlsCert, tlsKey); err != nil {
			return err
		}
	}

	// Create a listener based on the address type (either Unix socket or TCP)
	var listener net.Listener
	var err error
	if strings.HasPrefix(addr, "unix:") {
		socketPath := strings.TrimPrefix(addr, "unix:")
		listener, err = createUnixSocketFile(socketPath, conf.Server.UnixSocketPerm)
		if err != nil {
			return err
		}
	} else {
		addr = fmt.Sprintf("%s:%d", addr, port)
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("creating tcp listener: %w", err)
		}
	}

	// Start the server in a new goroutine and send an error signal to errC if there's an error
	errC := make(chan error)
	go func() {
		var err error
		if tlsEnabled {
			// Start the HTTPS server
			log.Info("Starting server with TLS (HTTPS) enabled", "tlsCert", tlsCert, "tlsKey", tlsKey)
			err = server.ServeTLS(listener, tlsCert, tlsKey)
		} else {
			// Start the HTTP server
			err = server.Serve(listener)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errC <- err
		}
	}()

	// Measure server startup time
	startupTime := time.Since(consts.ServerStart)

	// Wait a short time to make sure the server has started successfully
	select {
	case err := <-errC:
		log.Error(ctx, "Could not start server. Aborting", err)
		return fmt.Errorf("starting server: %w", err)
	case <-time.After(50 * time.Millisecond):
		log.Info(ctx, "----> Navidrome server is ready!", "address", addr, "startupTime", startupTime, "tlsEnabled", tlsEnabled)
	}

	// Wait for a signal to terminate
	select {
	case err := <-errC:
		return fmt.Errorf("running server: %w", err)
	case <-ctx.Done():
		// If the context is done (i.e. the server should stop), proceed to shutting down the server
	}

	// Try to stop the HTTP server gracefully
	log.Info(ctx, "Stopping HTTP server")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server.SetKeepAlivesEnabled(false)
	if err := server.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Error(ctx, "Unexpected error in http.Shutdown()", err)
	}
	return nil
}

// createUnixSocketFile 创建 Unix socket 监听。
// 需先删除残留的旧 socket 文件，否则 bind 会失败；
// 权限位以八进制字符串配置，创建后再 chmod。
func createUnixSocketFile(socketPath string, socketPerm string) (net.Listener, error) {
	// Remove the socket file if it already exists
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing previous unix socket file: %w", err)
	}
	// Create listener
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("creating unix socket listener: %w", err)
	}
	// Converts the socketPerm to uint and updates the permission of the unix socket file
	perm, err := strconv.ParseUint(socketPerm, 8, 32)
	if err != nil {
		return nil, fmt.Errorf("parsing unix socket file permissions: %w", err)
	}
	err = os.Chmod(socketPath, os.FileMode(perm))
	if err != nil {
		return nil, fmt.Errorf("updating permission of unix socket file: %w", err)
	}
	return listener, nil
}

// initRoutes 构建路由与默认中间件栈。
//
// 中间件顺序有意为之：安全头与 CORS 最先，RequestID/RealIP 需在日志之前，
// Recoverer 要能兜住后续所有中间件的 panic，JWTVerifier 置于末尾以便后续鉴权使用。
// DevActivityPanel 的 SSE 端点单独分组：它是长连接，不能走 requestLogger。
func (s *Server) initRoutes() {
	s.appRoot = path.Join(conf.Server.BasePath, consts.URLPathUI)

	r := chi.NewRouter()

	defaultMiddlewares := chi.Middlewares{
		secureMiddleware(),
		corsHandler(),
		middleware.RequestID,
		realIPMiddleware,
		middleware.Recoverer,
		middleware.Heartbeat("/ping"),
		robotsTXT(ui.BuildAssets()),
		serverAddressMiddleware,
		clientUniqueIDMiddleware,
		compressMiddleware(),
		loggerInjector,
		JWTVerifier,
	}

	// Mount the Native API /events endpoint with all default middlewares, adding the authentication middlewares
	if conf.Server.DevActivityPanel {
		r.Group(func(r chi.Router) {
			r.Use(defaultMiddlewares...)
			r.Use(Authenticator(s.ds))
			r.Use(JWTRefresher)
			r.Handle(path.Join(conf.Server.BasePath, consts.URLPathNativeAPI, "events"), s.broker)
		})
	}

	// Configure the router with the default middlewares and requestLogger
	r.Group(func(r chi.Router) {
		r.Use(defaultMiddlewares...)
		r.Use(requestLogger)
		s.router = r
	})
}

// mountAuthenticationRoutes 挂载登录与创建管理员接口。
// 登录接口默认按 IP 限流以抵御暴力破解，关闭时给出显式告警。
func (s *Server) mountAuthenticationRoutes() chi.Router {
	r := s.router
	return r.Route(path.Join(conf.Server.BasePath, "/auth"), func(r chi.Router) {
		if conf.Server.AuthRequestLimit > 0 {
			log.Info("Login rate limit set", "requestLimit", conf.Server.AuthRequestLimit,
				"windowLength", conf.Server.AuthWindowLength)

			rateLimiter := httprate.LimitByIP(conf.Server.AuthRequestLimit, conf.Server.AuthWindowLength)
			r.With(rateLimiter).Post("/login", login(s.ds))
		} else {
			log.Warn("Login rate limit is disabled! Consider enabling it to be protected against brute-force attacks")

			r.Post("/login", login(s.ds))
		}
		r.Post("/createAdmin", createAdmin(s.ds))
	})
}

// Serve UI app assets
// mountRootRedirector 把根路径重定向到前端 UI 路径。
func (s *Server) mountRootRedirector() {
	r := s.router
	// Redirect root to UI URL
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.appRoot+"/", http.StatusFound)
	})
	r.Get(s.appRoot, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.appRoot+"/", http.StatusFound)
	})
}

// frontendAssetsHandler 提供前端静态资源。
// 首页需经 Index 处理器注入运行时配置，其余静态文件直接从内嵌 FS 读取。
func (s *Server) frontendAssetsHandler() http.Handler {
	r := chi.NewRouter()

	r.Handle("/", Index(s.ds, ui.BuildAssets()))
	r.Handle("/*", http.StripPrefix(s.appRoot, http.FileServer(http.FS(ui.BuildAssets()))))
	return r
}

// AbsoluteURL 把相对路径补全为绝对 URL。
// 配置了 BaseHost 时优先采用（反向代理场景下请求头中的 Host 可能不可靠），
// 否则回退到当前请求的 Host。
func AbsoluteURL(r *http.Request, u string, params url.Values) string {
	buildUrl, _ := url.Parse(u)
	if strings.HasPrefix(u, "/") {
		buildUrl.Path = path.Join(conf.Server.BasePath, buildUrl.Path)
		if conf.Server.BaseHost != "" {
			buildUrl.Scheme = cmp.Or(conf.Server.BaseScheme, "http")
			buildUrl.Host = conf.Server.BaseHost
		} else {
			buildUrl.Scheme = r.URL.Scheme
			buildUrl.Host = r.Host
		}
	}
	if len(params) > 0 {
		buildUrl.RawQuery = params.Encode()
	}
	return buildUrl.String()
}

// validateTLSCertificates validates the TLS certificate and key files before starting the server.
// It provides detailed error messages for common issues like encrypted private keys.
//
// validateTLSCertificates 在启动前校验证书与私钥。
// 提前校验是为了给出可读的错误提示——尤其是加密私钥这种常见误用，
// 标准库的报错信息难以让用户定位问题。
func validateTLSCertificates(certFile, keyFile string) error {
	// Read the key file to check for encryption
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("reading TLS key file: %w", err)
	}

	// Parse PEM blocks and check for encryption
	block, _ := pem.Decode(keyData)
	if block == nil {
		return errors.New("TLS key file does not contain a valid PEM block")
	}

	// Check for encrypted private key indicators
	if isEncryptedPEM(block, keyData) {
		return errors.New("TLS private key is encrypted (password-protected). " +
			"Navidrome does not support encrypted private keys. " +
			"Please decrypt your key using: openssl pkey -in <encrypted-key> -out <decrypted-key>")
	}

	// Try to load the certificate pair to validate it
	_, err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("loading TLS certificate/key pair: %w", err)
	}

	return nil
}

// isEncryptedPEM checks if a PEM block represents an encrypted private key.
//
// isEncryptedPEM 判断私钥是否被加密。
// 需覆盖三种情形：PKCS#8 的 ENCRYPTED PRIVATE KEY 类型、
// 传统格式的 Proc-Type 头，以及 pem.Decode 未能正确解析头部时的原文兜底匹配。
func isEncryptedPEM(block *pem.Block, rawData []byte) bool {
	// Check for PKCS#8 encrypted format (BEGIN ENCRYPTED PRIVATE KEY)
	if block.Type == "ENCRYPTED PRIVATE KEY" {
		return true
	}

	// Check for legacy encrypted format with Proc-Type header
	if block.Headers != nil {
		if procType, ok := block.Headers["Proc-Type"]; ok && strings.Contains(procType, "ENCRYPTED") {
			return true
		}
	}

	// Also check raw data for DEK-Info header (in case pem.Decode doesn't parse headers correctly)
	if bytes.Contains(rawData, []byte("DEK-Info:")) || bytes.Contains(rawData, []byte("Proc-Type: 4,ENCRYPTED")) {
		return true
	}

	return false
}
