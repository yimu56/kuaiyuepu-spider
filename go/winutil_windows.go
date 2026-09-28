//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleOutputCP    = kernel32.NewProc("SetConsoleOutputCP")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

func init() {
	// 把控制台代码页切成 UTF-8，中文输出才不会是乱码
	procSetConsoleOutputCP.Call(uintptr(65001))
}

// setHideWindow 让子进程（浏览器）不弹出控制台窗口。
func setHideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// startedByDoubleClick 判断程序是否由「双击」启动。
// 双击会新建一个独立控制台，里面只有本进程自己（n==1）。
// n==0 表示根本没有控制台（输出被重定向 / 以服务方式启动），此时绝不能暂停。
func startedByDoubleClick() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}
