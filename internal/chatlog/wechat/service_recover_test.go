package wechat

import (
	"strings"
	"testing"
)

// --- recoverDecryptPanic 单测 ---
//
// 项目约定：所有 autodecrypt goroutine 第一条 defer 必须是 s.recoverDecryptPanic(name)。
// 本组测试锁定 helper 的行为契约，间接保护所有使用它的 goroutine（waitAndProcess
// 已经使用；后续 Stage G 的 firstFullDecrypt 也会使用）。
//
// REGRESSION ANCHOR: 用户历史上有 handle 泄漏 + context 死锁经历，长期运行稳定性
// 容忍度低。裸跑的 goroutine panic 会炸进程 —— 本 helper 是兜底。

func TestRecoverDecryptPanic_CatchesPanic_CallsHandler(t *testing.T) {
	var handlerErr error
	svc := NewService(&mockConfig{})
	svc.SetAutoDecryptErrorHandler(func(err error) {
		handlerErr = err
	})

	func() {
		defer svc.recoverDecryptPanic("test-goroutine")
		panic("simulated panic message")
	}()

	if handlerErr == nil {
		t.Fatal("expected errorHandler to be called, got nil")
	}
	if !strings.Contains(handlerErr.Error(), "test-goroutine") {
		t.Errorf("err should mention op name, got: %v", handlerErr)
	}
	if !strings.Contains(handlerErr.Error(), "simulated panic message") {
		t.Errorf("err should mention panic value, got: %v", handlerErr)
	}
	if !strings.Contains(handlerErr.Error(), "panic") {
		t.Errorf("err should label as panic, got: %v", handlerErr)
	}
}

func TestRecoverDecryptPanic_NoPanic_NoHandlerCall(t *testing.T) {
	handlerCalled := false
	svc := NewService(&mockConfig{})
	svc.SetAutoDecryptErrorHandler(func(err error) {
		handlerCalled = true
	})

	func() {
		defer svc.recoverDecryptPanic("test-goroutine")
		// no panic, 正常返回
	}()

	if handlerCalled {
		t.Error("handler should not be called when no panic occurred")
	}
}

func TestRecoverDecryptPanic_HandlerNil_DoesNotCrash(t *testing.T) {
	// Edge case：未设置 errorHandler 时 panic 不应二次 panic（nil deref）。
	svc := NewService(&mockConfig{})
	// 刻意不调用 SetAutoDecryptErrorHandler

	func() {
		defer svc.recoverDecryptPanic("test-goroutine")
		panic("should be swallowed silently")
	}()
	// 能走到这里就是成功
}

func TestRecoverDecryptPanic_NonStringPanicValue(t *testing.T) {
	// Edge case：panic 的值不是 string 时（比如 error / int / struct）
	var handlerErr error
	svc := NewService(&mockConfig{})
	svc.SetAutoDecryptErrorHandler(func(err error) {
		handlerErr = err
	})

	func() {
		defer svc.recoverDecryptPanic("test-goroutine")
		panic(42) // int 类型
	}()

	if handlerErr == nil {
		t.Fatal("expected handler called even for non-string panic")
	}
	if !strings.Contains(handlerErr.Error(), "42") {
		t.Errorf("err should contain panic value '42', got: %v", handlerErr)
	}
}
