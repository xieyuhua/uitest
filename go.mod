module uitest-server

go 1.26.0

// 直接依赖：全部纯 Go 实现（不需要 CGO / 本地客户端库），Windows 与 Linux 同一份代码
require (
	github.com/go-sql-driver/mysql v1.10.1 // MySQL 驱动（接口采集落库）
	github.com/gorilla/websocket v1.5.3    // CDP WebSocket 客户端（内置浏览器）
	github.com/lib/pq v1.12.3              // PostgreSQL 驱动（接口采集落库）
	github.com/xuri/excelize/v2 v2.11.1    // Excel .xlsx 读写（接口采集落库）
	modernc.org/sqlite v1.61.1             // SQLite 驱动（纯 Go，默认落库）
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/richardlehane/mscfb v1.0.9 // indirect
	github.com/richardlehane/msoleps v1.0.6 // indirect
	github.com/tiendc/go-deepcopy v1.7.2 // indirect
	github.com/xuri/efp v0.0.3-0.20260924042702-217b81952dec // indirect
	github.com/xuri/nfp v0.0.2-0.20250530014748-2ddeb826f9a9 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
