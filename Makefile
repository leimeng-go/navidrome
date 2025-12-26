# 从 go.mod 文件中提取 Go 版本号（例如：1.23）
# 执行流程：
#   1. grep "^go " go.mod - 在 go.mod 中搜索以 "go " 开头的行（^ 表示行首）
#      示例输入：go.mod 中的 "go 1.25" 行
#      示例输出："go 1.25"
#   2. | - 管道符，将前一个命令的输出作为下一个命令的输入
#   3. cut -f 2 -d ' ' - 按空格分隔并提取第2个字段
#      -d ' '  : 指定分隔符为空格
#      -f 2    : 提取第2个字段（字段从1开始计数）
#      输入："go 1.25"
#      输出："1.25"
GO_VERSION=$(shell grep "^go " go.mod | cut -f 2 -d ' ')

# 从 .nvmrc 文件中读取 Node.js 版本要求
# cat - 读取并输出文件内容
# 示例：如果 .nvmrc 内容为 "v20.0.0"，则 NODE_VERSION=v20.0.0
NODE_VERSION=$(shell cat .nvmrc)

# 检测是否在 Git 仓库中（通过 .git/HEAD 文件是否存在判断）
ifneq ("$(wildcard .git/HEAD)","")
# 如果在 Git 仓库中，获取当前提交的短 SHA 值
GIT_SHA=$(shell git rev-parse --short HEAD)
# 获取最新的 Git 标签并添加 -SNAPSHOT 后缀
GIT_TAG=$(shell git describe --tags `git rev-list --tags --max-count=1`)-SNAPSHOT
else
# 如果不在 Git 仓库中（如从源码压缩包构建），使用固定标识
GIT_SHA=source_archive
# 从当前目录名提取版本号（例如：navidrome-1.2.3 → v1.2.3）
GIT_TAG=$(patsubst navidrome-%,v%,$(notdir $(PWD)))-SNAPSHOT
endif

# 定义所有支持的编译平台（?= 表示可以被命令行参数覆盖）
SUPPORTED_PLATFORMS ?= linux/amd64,linux/arm64,linux/arm/v5,linux/arm/v6,linux/arm/v7,linux/386,darwin/amd64,darwin/arm64,windows/amd64,windows/386
# 筛选出 Linux 平台用于 Docker 镜像构建（排除 arm/v5，因为 Docker 不支持）
# 执行流程：
#   1. echo $(SUPPORTED_PLATFORMS) - 输出所有平台（逗号分隔）
#      输出："linux/amd64,linux/arm64,darwin/amd64,windows/amd64,..."
#   2. tr ',' '\n' - 将逗号替换为换行符（translate 字符）
#      输出：每个平台占一行
#   3. grep "linux" - 只保留包含 "linux" 的行
#   4. grep -v "arm/v5" - 排除包含 "arm/v5" 的行（-v 表示反向匹配）
#   5. tr '\n' ',' - 将换行符替换回逗号
#   6. sed 's/,$$//' - 删除末尾的逗号（$ 表示行尾，$$ 是 Makefile 中的转义）
#      最终输出："linux/amd64,linux/arm64,linux/arm/v6,..."
IMAGE_PLATFORMS ?= $(shell echo $(SUPPORTED_PLATFORMS) | tr ',' '\n' | grep "linux" | grep -v "arm/v5" | tr '\n' ',' | sed 's/,$$//')
# 默认编译平台（可通过 make PLATFORMS="linux/amd64" 覆盖）
PLATFORMS ?= $(SUPPORTED_PLATFORMS)
# Docker 镜像的默认标签
DOCKER_TAG ?= deluan/navidrome:develop

# 跨平台编译时使用的 TagLib 版本（音频标签库）
# 来源：https://github.com/navidrome/cross-taglib
CROSS_TAGLIB_VERSION ?= 2.1.1-1
# golangci-lint 代码检查工具的版本
GOLANGCI_LINT_VERSION ?= v2.7.2

# 查找所有前端源文件（排除 build 产物和 node_modules）
# 用于判断前端是否需要重新构建
# find 命令详解：
#   ui              - 在 ui 目录中搜索
#   -type f         - 只查找文件（不包括目录）
#   -not -path "ui/build/*"       - 排除 ui/build 目录下的所有文件
#   -not -path "ui/node_modules/*" - 排除 ui/node_modules 目录下的所有文件
# 这样可以确保只有源文件变化时才触发前端重新构建
UI_SRC_FILES := $(shell find ui -type f -not -path "ui/build/*" -not -path "ui/node_modules/*")

# 首次环境搭建：检查环境 → 下载依赖 → 安装代码检查工具 → 设置 Git 钩子 → 安装前端依赖
setup: check_env download-deps install-golangci-lint setup-git ##@1_Run_First Install dependencies and prepare development environment
	@echo Downloading Node dependencies...
	@(cd ./ui && npm ci)  # npm ci 比 npm install 更适合 CI/CD（根据 package-lock.json 精确安装）
.PHONY: setup  # 声明为伪目标，不对应实际文件

# 启动开发模式：同时启动前后端，支持热重载
dev: check_env   ##@Development Start Navidrome in development mode, with hot-reload for both frontend and backend
	# ND_ENABLEINSIGHTSCOLLECTOR="false" - 禁用遥测数据收集
	# npx foreman - 使用 foreman 进程管理器
	# -j Procfile.dev - 指定进程配置文件
	# -p 4533 - 前端端口 4533，后端自动使用 4633（+100）
	ND_ENABLEINSIGHTSCOLLECTOR="false" npx foreman -j Procfile.dev -p 4533 start
.PHONY: dev

# 只启动后端开发服务器（需要先构建前端）
server: check_go_env buildjs ##@Development Start the backend in development mode
	# reflex - Go 的文件监控和自动重启工具（类似 nodemon）
	# -d none - 禁用默认延迟，立即重启
	# -c reflex.conf - 使用配置文件定义监控规则
	@ND_ENABLEINSIGHTSCOLLECTOR="false" go tool reflex -d none -c reflex.conf
.PHONY: server

# 停止所有开发服务器
stop: ##@Development Stop development servers (UI and backend)
	@echo "Stopping development servers..."
	@-pkill -f "vite"  # 停止前端 Vite 服务器（- 前缀表示忽略错误）
	@-pkill -f "go tool reflex.*reflex.conf"  # 停止 reflex 监控进程
	@-pkill -f "go run.*netgo"  # 停止 Go 运行时进程
	@echo "Development servers stopped."
.PHONY: stop

# 监控模式运行测试：代码变化时自动重新运行测试
watch: ##@Development Start Go tests in watch mode (re-run when code changes)
	# ginkgo - Go 的 BDD 测试框架
	# watch - 监控文件变化
	# -tags=netgo - 使用纯 Go 网络库（静态编译）
	# -notify - 显示系统通知
	# ./... - 递归测试所有子目录
	go tool ginkgo watch -tags=netgo -notify ./...
.PHONY: watch

# 默认测试所有包（可通过 make test PKG=./server 指定特定包）
PKG ?= ./...
# 运行 Go 测试
test: ##@Development Run Go tests. Use PKG variable to specify packages to test, e.g. make test PKG=./server
	go test -tags netgo $(PKG)
.PHONY: test

# 运行所有测试：竞态检测 + 国际化验证 + 前端测试
testall: test-race test-i18n test-js ##@Development Run Go and JS tests
.PHONY: testall

# 运行 Go 测试并启用竞态检测器（检测并发安全问题）
test-race: ##@Development Run Go tests with race detector
	# -race - 启用竞态检测器（会降低性能但能发现并发 bug）
	# -shuffle=on - 随机化测试顺序（避免依赖测试执行顺序）
	go test -tags netgo -race -shuffle=on  $(PKG)
.PHONY: test-race

# 运行前端测试（使用 Vitest）
test-js: ##@Development Run JS tests
	@(cd ./ui && npm run test)
.PHONY: test-js

# 验证所有翻译文件的完整性和正确性
test-i18n: ##@Development Validate all translations files
	./.github/workflows/validate-translations.sh 
.PHONY: test-i18n

# 智能安装 golangci-lint：仅在不存在或版本不匹配时安装
install-golangci-lint: ##@Development Install golangci-lint if not present
	@INSTALL=false; \
	# 检查 golangci-lint 是否已安装（包括 ./bin 目录）
	# PATH=$$PATH:./bin - 临时将 ./bin 加入 PATH 环境变量（$$ 是 Makefile 中的转义）
	# which golangci-lint - 查找命令的完整路径
	# > /dev/null - 将标准输出重定向到 /dev/null（丢弃）
	# 2>&1 - 将标准错误（文件描述符2）重定向到标准输出（文件描述符1）
	if PATH=$$PATH:./bin which golangci-lint > /dev/null 2>&1; then \
		# 提取当前版本号
		# grep -oE '[0-9]+\.[0-9]+\.[0-9]+' - 使用扩展正则表达式匹配版本号
		#   -o : 只输出匹配的部分（而不是整行）
		#   -E : 使用扩展正则表达式
		# head -n1 - 只取第一行（防止多个匹配）
		CURRENT_VERSION=$$(PATH=$$PATH:./bin golangci-lint version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1); \
		# 提取要求的版本号（去掉 v 前缀）
		# sed 's/^v//' - 替换命令，将行首（^）的 v 字符替换为空
		#   例如：v2.7.2 → 2.7.2
		REQUIRED_VERSION=$$(echo "$(GOLANGCI_LINT_VERSION)" | sed 's/^v//'); \
		# 比较版本号，不一致则重新安装
		if [ "$$CURRENT_VERSION" != "$$REQUIRED_VERSION" ]; then \
			echo "Found golangci-lint $$CURRENT_VERSION, but $$REQUIRED_VERSION is required. Reinstalling..."; \
			rm -f ./bin/golangci-lint; \
			INSTALL=true; \
		fi; \
	else \
		# 未安装，标记需要安装
		INSTALL=true; \
	fi; \
	# 如果需要安装，从官方脚本下载
	if [ "$$INSTALL" = "true" ]; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s $(GOLANGCI_LINT_VERSION); \
	fi
.PHONY: install-golangci-lint

# 运行 Go 代码检查（自动安装 golangci-lint）
lint: install-golangci-lint ##@Development Lint Go code
	# -v - 显示详细输出
	# --timeout 5m - 设置超时时间为 5 分钟
	PATH=$$PATH:./bin golangci-lint run -v --timeout 5m
.PHONY: lint

# 运行所有代码检查：Go + 前端格式化 + 前端 ESLint
lintall: lint ##@Development Lint Go and JS code
	# 检查前端代码格式（如果失败，提示使用 prettier 修复）
	@(cd ./ui && npm run check-formatting) || (echo "\n\nPlease run 'npm run prettier' to fix formatting issues." && exit 1)
	# 运行 ESLint 检查前端代码
	@(cd ./ui && npm run lint)
.PHONY: lintall

# 格式化所有代码：前端 + Go
format: ##@Development Format code
	# 使用 Prettier 格式化前端代码
	@(cd ./ui && npm run prettier)
	# 使用 goimports 格式化 Go 代码（自动整理 import）
	# 排除自动生成的文件：_gen.go 和 .pb.go
	@go tool goimports -w `find . -name '*.go' | grep -v _gen.go$$ | grep -v .pb.go$$`
	# 整理 go.mod，移除未使用的依赖
	@go mod tidy
.PHONY: format

# 更新依赖注入代码（使用 google/wire）
wire: check_go_env ##@Development Update Dependency Injection
	# wire gen - 根据 wire.go 文件生成依赖注入代码
	go tool wire gen -tags=netgo ./...
.PHONY: wire

# 更新快照测试（用于测试 API 响应的结构是否变化）
snapshots: ##@Development Update (GoLang) Snapshot tests
	# UPDATE_SNAPSHOTS=true - 更新快照而不是比较
	UPDATE_SNAPSHOTS=true go tool ginkgo ./server/subsonic/responses/...
.PHONY: snapshots

# 创建新的 SQL 数据库迁移文件
# 用法：make migration-sql name=add_user_table
migration-sql: ##@Development Create an empty SQL migration file
	@if [ -z "${name}" ]; then echo "Usage: make migration-sql name=name_of_migration_file"; exit 1; fi
	# 使用 goose 工具创建迁移文件
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir db/migrations create ${name} sql
.PHONY: migration

# 创建新的 Go 数据库迁移文件（用于复杂迁移逻辑）
# 用法：make migration-go name=migrate_data
migration-go: ##@Development Create an empty Go migration file
	@if [ -z "${name}" ]; then echo "Usage: make migration-go name=name_of_migration_file"; exit 1; fi
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir db/migrations create ${name}
.PHONY: migration

# setup-dev 是 setup 的别名（向后兼容）
setup-dev: setup
.PHONY: setup-dev

# 设置 Git 钩子：pre-commit 和 pre-push
setup-git: ##@Development Setup Git hooks (pre-commit and pre-push)
	@echo Setting up git hooks
	@mkdir -p .git/hooks
	# 创建符号链接，指向 git/ 目录下的钩子脚本
	# 这样更新钩子时无需重新复制
	@(cd .git/hooks && ln -sf ../../git/* .)
.PHONY: setup-git

# 构建完整项目：前端 + 后端
build: check_go_env buildjs ##@Build Build the project
	# -ldflags - 链接时注入变量（用于显示版本信息）
	# -X - 设置字符串变量的值
	# -tags=netgo - 使用纯 Go 网络库，避免 cgo 依赖（生成静态二进制）
	go build -ldflags="-X github.com/navidrome/navidrome/consts.gitSha=$(GIT_SHA) -X github.com/navidrome/navidrome/consts.gitTag=$(GIT_TAG)" -tags=netgo
.PHONY: build

# buildall 已废弃，使用 build 代替
buildall: deprecated build
.PHONY: buildall

# 构建调试版本：禁用优化和内联，方便使用 delve 调试
debug-build: check_go_env buildjs ##@Build Build the project (with remote debug on)
	# -gcflags="all=-N -l" - 编译器参数
	# -N - 禁用优化
	# -l - 禁用内联（方便设置断点）
	go build -gcflags="all=-N -l" -ldflags="-X github.com/navidrome/navidrome/consts.gitSha=$(GIT_SHA) -X github.com/navidrome/navidrome/consts.gitTag=$(GIT_TAG)" -tags=netgo
.PHONY: debug-build

# 只构建前端（依赖 ui/build/index.html 规则）
buildjs: check_node_env ui/build/index.html ##@Build Build only frontend
.PHONY: buildjs

# 使用 Docker 构建前端（无需本地安装 Node.js）
docker-buildjs: ##@Build Build only frontend using Docker
	# --output - 将构建产物复制到本地
	# --target ui-bundle - 只构建 Dockerfile 中的 ui-bundle 阶段
	docker build --output "./ui" --target ui-bundle .
.PHONY: docker-buildjs

# 前端构建规则：当源文件变化时触发
ui/build/index.html: $(UI_SRC_FILES)
	# 进入 ui 目录执行 Vite 构建
	@(cd ./ui && npm run build)

# 列出所有支持的跨平台编译目标
docker-platforms: ##@Cross_Compilation List supported platforms
	@echo "Supported platforms:"
	# 格式化输出平台列表：
	# tr ',' '\n' - translate 字符，将逗号替换为换行符
	#   输入："linux/amd64,linux/arm64,darwin/amd64"
	#   输出：每个平台占一行
	# sort - 按字母顺序排序
	# sed 's/^/    /' - stream editor，在每行行首（^）添加 4 个空格
	#   效果：    linux/amd64
	#         darwin/amd64
	@echo "$(SUPPORTED_PLATFORMS)" | tr ',' '\n' | sort | sed 's/^/    /'
	@echo "\nUsage: make PLATFORMS=\"linux/amd64\" docker-build"
	@echo "       make IMAGE_PLATFORMS=\"linux/amd64\" docker-image"
.PHONY: docker-platforms

# 使用 Docker Buildx 进行跨平台编译
# 示例：make docker-build PLATFORMS="linux/amd64,linux/arm64"
docker-build: ##@Cross_Compilation Cross-compile for any supported platform (check `make docker-platforms`)
	# buildx - Docker 的多平台构建工具
	# --platform - 指定目标平台（可多个）
	# --build-arg - 传递构建参数
	# --output - 将编译产物输出到本地目录
	# --target binary - 只构建到 binary 阶段（不构建完整镜像）
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg GIT_TAG=${GIT_TAG} \
		--build-arg GIT_SHA=${GIT_SHA} \
		--build-arg CROSS_TAGLIB_VERSION=${CROSS_TAGLIB_VERSION} \
		--output "./binaries" --target binary .
.PHONY: docker-build

# 构建 Docker 镜像（多平台）
# 示例：make docker-image DOCKER_TAG="myregistry/navidrome:v1.0.0"
docker-image: ##@Cross_Compilation Build Docker image, tagged as `deluan/navidrome:develop`, override with DOCKER_TAG var. Use IMAGE_PLATFORMS to specify target platforms
	# 验证平台：Docker 镜像不支持 Windows、macOS 和 ARMv5
	@echo $(IMAGE_PLATFORMS) | grep -q "windows" && echo "ERROR: Windows is not supported for Docker builds" && exit 1 || true
	@echo $(IMAGE_PLATFORMS) | grep -q "darwin" && echo "ERROR: macOS is not supported for Docker builds" && exit 1 || true
	@echo $(IMAGE_PLATFORMS) | grep -q "arm/v5" && echo "ERROR: Linux ARMv5 is not supported for Docker builds" && exit 1 || true
	# 构建多平台镜像并打标签
	docker buildx build \
		--platform $(IMAGE_PLATFORMS) \
		--build-arg GIT_TAG=${GIT_TAG} \
		--build-arg GIT_SHA=${GIT_SHA} \
		--build-arg CROSS_TAGLIB_VERSION=${CROSS_TAGLIB_VERSION} \
		--tag $(DOCKER_TAG) .
.PHONY: docker-image

# 构建 Windows MSI 安装程序（32位 + 64位）
docker-msi: ##@Cross_Compilation Build MSI installer for Windows
	# 先编译 Windows 二进制文件
	make docker-build PLATFORMS=windows/386,windows/amd64
	# 构建 MSI 打包工具镜像（使用 WiX Toolset）
	DOCKER_CLI_HINTS=false docker build -q -t navidrome-msi-builder -f release/wix/msitools.dockerfile .
	@rm -rf binaries/msi
	# 在容器中运行 MSI 构建脚本
	docker run -it --rm -v $(PWD):/workspace -v $(PWD)/binaries:/workspace/binaries -e GIT_TAG=${GIT_TAG} \
		navidrome-msi-builder sh -c "release/wix/build_msi.sh /workspace 386 && release/wix/build_msi.sh /workspace amd64"
	# 显示生成的 MSI 文件大小
	@du -h binaries/msi/*.msi
.PHONY: docker-msi

# 运行指定的 Docker 镜像
# 用法：make run-docker tag=deluan/navidrome:latest
run-docker: ##@Development Run a Navidrome Docker image. Usage: make run-docker tag=<tag>
	@if [ -z "$(tag)" ]; then echo "Usage: make run-docker tag=<tag>"; exit 1; fi
	# 创建临时数据目录（根据镜像标签命名，避免冲突）
	@TAG_DIR="tmp/$$(echo '$(tag)' | tr '/:' '_')"; mkdir -p "$$TAG_DIR"; \
    VOLUMES="-v $(PWD)/$$TAG_DIR:/data"; \
	# 如果存在 navidrome.toml 配置文件，挂载它
	if [ -f navidrome.toml ]; then \
		VOLUMES="$$VOLUMES -v $(PWD)/navidrome.toml:/data/navidrome.toml:ro"; \
			# 从配置文件中提取音乐目录路径
		# grep '^MusicFolder' navidrome.toml - 匹配以 MusicFolder 开头的行
		# head -n1 - 只取第一个匹配结果
		# sed 's/.*= *"//' - 删除 = " 之前的所有内容
		#   .*  : 匹配任意字符
		#   = * : 匹配等号和后面的空格（* 表示0个或多个）
		#   \"  : 匹配引号
		# sed 's/".*//' - 删除引号及之后的所有内容
		# 示例：MusicFolder = "/path/to/music" → /path/to/music
		MUSIC_FOLDER=$$(grep '^MusicFolder' navidrome.toml | head -n1 | sed 's/.*= *"//' | sed 's/".*//'); \
		# 如果音乐目录存在，挂载为只读
		if [ -n "$$MUSIC_FOLDER" ] && [ -d "$$MUSIC_FOLDER" ]; then \
		  VOLUMES="$$VOLUMES -v $$MUSIC_FOLDER:/music:ro"; \
	  	fi; \
	fi; \
	# 运行容器：映射端口 4533，自动清理
	echo "Running: docker run --rm -p 4533:4533 $$VOLUMES $(tag)"; docker run --rm -p 4533:4533 $$VOLUMES $(tag)
.PHONY: run-docker

# 使用 GoReleaser 打包所有平台的发布版本
package: docker-build ##@Cross_Compilation Create binaries and packages for ALL supported platforms
	# 检查 goreleaser 是否已安装
	@if [ -z `which goreleaser` ]; then echo "Please install goreleaser first: https://goreleaser.com/install/"; exit 1; fi
	# --clean - 清理之前的构建产物
	# --skip=publish - 不发布到仓库（仅本地构建）
	# --snapshot - 快照版本（不需要 Git 标签）
	goreleaser release -f release/goreleaser.yml --clean --skip=publish --snapshot
.PHONY: package

# 从 Navidrome 演示实例下载免费音乐（用于开发测试）
get-music: ##@Development Download some free music from Navidrome's demo instance
	mkdir -p music
	# 下载多个 CC 授权的音乐专辑
	( cd music; \
	curl "https://demo.navidrome.org/rest/download?u=demo&p=demo&f=json&v=1.8.0&c=dev_download&id=2Y3qQA6zJC3ObbBrF9ZBoV" > brock.zip; \
	curl "https://demo.navidrome.org/rest/download?u=demo&p=demo&f=json&v=1.8.0&c=dev_download&id=04HrSORpypcLGNUdQp37gn" > back_on_earth.zip; \
	curl "https://demo.navidrome.org/rest/download?u=demo&p=demo&f=json&v=1.8.0&c=dev_download&id=5xcMPJdeEgNrGtnzYbzAqb" > ugress.zip; \
	curl "https://demo.navidrome.org/rest/download?u=demo&p=demo&f=json&v=1.8.0&c=dev_download&id=1jjQMAZrG3lUsJ0YH6ZRS0" > voodoocuts.zip; \
	# 解压所有下载的 zip 文件（-n 不覆盖已存在的文件）
	for file in *.zip; do unzip -n $${file}; done )
	@echo "Done. Remember to set your MusicFolder to ./music"
.PHONY: get-music


##########################################
#### 杂项工具

# 清理所有构建产物
clean:
	@rm -rf ./binaries ./dist ./ui/build/*
	# 保留 .gitkeep 文件以维持 Git 目录结构
	@touch ./ui/build/.gitkeep
.PHONY: clean

# 发布新版本：创建 Git 标签并推送
# 用法：make release V=1.2.3
release:
	# 验证版本号格式（必须是 X.Y.Z 格式）
	@if [[ ! "${V}" =~ ^[0-9]+\.[0-9]+\.[0-9]+.*$$ ]]; then echo "Usage: make release V=X.X.X"; exit 1; fi
	# 整理依赖
	go mod tidy
	# 检查是否有未提交的更改
	@if [ -n "`git status -s`" ]; then echo "\n\nThere are pending changes. Please commit or stash first"; exit 1; fi
	# 运行所有检查和测试
	make pre-push
	# 创建版本标签
	git tag v${V}
	# 推送标签到远程仓库（--no-verify 跳过 Git 钩子）
	git push origin v${V} --no-verify
.PHONY: release

# 下载 Go 模块依赖
download-deps:
	@echo Downloading Go dependencies...
	# 下载所有依赖到本地缓存
	@go mod download
	# 整理 go.mod，移除不需要的依赖（恢复 download 可能的更改）
	@go mod tidy # To revert any changes made by the `go mod download` command
.PHONY: download-deps

# 检查 Go 和 Node.js 环境
check_env: check_go_env check_node_env
.PHONY: check_env

# 检查 Go 环境和版本
check_go_env:
	# 检查 go 命令是否存在
	@(hash go) || (echo "\nERROR: GO environment not setup properly!\n"; exit 1)
	# 版本比较逻辑（详细步骤）：
	# 1. go version | cut -d ' ' -f 3 | cut -c3-
	#    go version     : 输出 "go version go1.23.1 linux/amd64"
	#    cut -d ' ' -f 3 : 按空格分隔，取第3个字段 "go1.23.1"
	#    cut -c3-       : 从第3个字符开始截取（去掉 "go" 前缀）→ "1.23.1"
	# 2. echo "$(GO_VERSION) $$current_go_version"
	#    输出两个版本号，例如："1.25 1.23.1"
	# 3. tr ' ' '\n'
	#    将空格替换为换行，每个版本号占一行
	# 4. sort -V
	#    按版本号排序（-V 表示 version sort，会正确处理 1.9 < 1.10）
	# 5. tail -1
	#    取最后一行（最大的版本号）
	# 6. grep -q "^$${current_go_version}$$"
	#    -q          : 静默模式，不输出内容，只返回退出码
	#    ^...$$      : 匹配整行（^ 行首，$$ 行尾，$$ 是 Makefile 转义）
	#    如果最大版本 = 当前版本，说明版本符合要求
	# 7. || - 逻辑或，如果 grep 失败（当前版本过低），执行后面的命令
	@current_go_version=`go version | cut -d ' ' -f 3 | cut -c3-` && \
		echo "$(GO_VERSION) $$current_go_version" | \
		tr ' ' '\n' | sort -V | tail -1 | \
		grep -q "^$${current_go_version}$$" || \
		(echo "\nERROR: Please upgrade your GO version\nThis project requires at least the version $(GO_VERSION)"; exit 1)
.PHONY: check_go_env

# 检查 Node.js 环境和版本（逻辑同上）
check_node_env:
	@(hash node) || (echo "\nERROR: Node environment not setup properly!\n"; exit 1)
	@current_node_version=`node --version` && \
		echo "$(NODE_VERSION) $$current_node_version" | \
		tr ' ' '\n' | sort -V | tail -1 | \
		grep -q "^$${current_node_version}$$" || \
		(echo "\nERROR: Please check your Node version. Should be at least $(NODE_VERSION)\n"; exit 1)
.PHONY: check_node_env

# Git pre-push 钩子：推送前运行所有检查和测试
pre-push: lintall testall
.PHONY: pre-push

# 废弃警告占位符
deprecated:
	@echo "WARNING: This target is deprecated and will be removed in future releases. Use 'make build' instead."
.PHONY: deprecated

# 从 plugins/api/api.proto 生成 Go 代码（使用 protobuf）
plugin-gen: check_go_env ##@Development Generate Go code from plugins protobuf files
	# go generate 会执行源码中的 //go:generate 指令
	go generate ./plugins/...
.PHONY: plugin-gen

# 构建所有示例插件（WASM）
plugin-examples: check_go_env ##@Development Build all example plugins
	# $(MAKE) -C - 在指定目录执行 make
	$(MAKE) -C plugins/examples clean all
.PHONY: plugin-examples

# 清理所有插件构建产物
plugin-clean: check_go_env ##@Development Clean all plugins
	$(MAKE) -C plugins/examples clean
	$(MAKE) -C plugins/testdata clean
.PHONY: plugin-clean

# 构建所有测试插件
plugin-tests: check_go_env ##@Development Build all test plugins
	$(MAKE) -C plugins/testdata clean all
.PHONY: plugin-tests

# 默认目标：运行 help
.DEFAULT_GOAL := help

# Perl 脚本：解析 Makefile 中的 ##@ 注释生成帮助文档
# 正则匹配格式：target: ##@Category Description
HELP_FUN = \
	%help; while(<>){push@{$$help{$$2//'options'}},[$$1,$$3] \
	if/^([\w-_]+)\s*:.*\#\#(?:@(\w+))?\s(.*)$$/}; \
	print"$$_:\n", map"  $$_->[0]".(" "x(20-length($$_->[0])))."$$_->[1]\n",\
	@{$$help{$$_}},"\n" for sort keys %help; \

# 显示帮助信息（执行 make 或 make help）
help: ##@Miscellaneous Show this help
	@echo "Usage: make [target] ...\n"
	# 使用 Perl 脚本解析 Makefile 并生成分组帮助
	@perl -e '$(HELP_FUN)' $(MAKEFILE_LIST)
