// Package singleton 提供按类型区分的泛型单例。
package singleton

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/navidrome/navidrome/log"
)

var (
	instances = map[string]interface{}{}
	pending   = map[string]chan struct{}{}
	lock      sync.RWMutex
)

// GetInstance 返回类型 T 的单例，不存在时用 constructor 创建。
//
// 以类型名为键，故同一类型全局只会有一个实例。
// 三段式加锁：先读锁快速命中（绝大多数调用走这条路径），
// 未命中再加写锁复查，并用 pending 通道让并发的其他调用方等待，
// 保证构造函数只执行一次——构造往往有副作用（建连接、起协程），重复执行代价很大。
func GetInstance[T any](constructor func() T) T {
	var v T
	name := reflect.TypeOf(v).String()

	// First check with read lock
	lock.RLock()
	if instance, ok := instances[name]; ok {
		defer lock.RUnlock()
		return instance.(T)
	}
	lock.RUnlock()

	// Now check if someone is already creating this type
	lock.Lock()

	// Check again with the write lock - someone might have created it
	if instance, ok := instances[name]; ok {
		lock.Unlock()
		return instance.(T)
	}

	// Check if creation is pending
	wait, isPending := pending[name]
	if !isPending {
		// We'll be the one creating it
		pending[name] = make(chan struct{})
		wait = pending[name]
	}
	lock.Unlock()

	// If someone else is creating it, wait for them
	if isPending {
		<-wait // Wait for creation to complete

		// Now it should be in the instances map
		lock.RLock()
		defer lock.RUnlock()
		return instances[name].(T)
	}

	// We're responsible for creating the instance
	newInstance := constructor()

	// Store it and signal other goroutines
	lock.Lock()
	instances[name] = newInstance
	close(wait)           // Signal that creation is complete
	delete(pending, name) // Clean up
	log.Trace("Created new singleton", "type", name, "instance", fmt.Sprintf("%+v", newInstance))
	lock.Unlock()

	return newInstance
}
