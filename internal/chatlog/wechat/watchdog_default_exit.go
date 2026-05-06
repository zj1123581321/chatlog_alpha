package wechat

import "os"

// defaultExit 在 Watchdog.Exit 没注入时落到 os.Exit。
// 抽出来是为了让真生产路径走 os.Exit，而测试通过 Watchdog.Exit 字段覆盖即可，
// 不需要替换包级 var（测试就直接注入 Exit 字段，从不会走到这里）。
func defaultExit(code int) {
	os.Exit(code)
}
