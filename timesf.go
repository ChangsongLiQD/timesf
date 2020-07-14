// singlecache包是模仿golang的singleflight包，之所以进行了一定的重写的原因
// 是因为普通的单飞包，必须手动的去Forget结果。假如我查缓存，然后缓存过期了，我
// 再去查缓存，会拿到和之前一样的结果。但是假如这段时间数据已经进行了变更的话，
// 就从新去查数据库。
package singlecache

import (
	"math"
	"sync"
	"time"
)

// call 是单飞的调用
type call struct {
	wg sync.WaitGroup

	// 结果值和错误，只有当等待组完成后，只会写一次。
	val interface{}
	err error

	// forgotten 标识是否已经选择遗忘了调用的结果。当进行单飞的过程中，可以使用
	// 此标签来辨别次
	forgotten bool

	// 重复数量和管道，在等待组还没完成之前，这两个字段在单飞的进行中，当拿到
	// 锁时进行读和写操作。拿到锁之后，这两个字段将只读不写。
	dups  int
	chans []chan<- Result
}

// Group 标识一个工作类，并且进行管理命名空间。其包含调用结果和获得调用结果的毫秒
// 时间戳。其可以进行对重复请求的抑制。
type Group struct {
	mu sync.Mutex       // protects m
	m  map[string]*call // lazily initialized
	t  map[string]int64 // valid time
}

// Result 保存DO方法的结果，因此Do方法可以通过管道来进行传输。
type Result struct {
	Val    interface{}
	Err    error
	Shared bool
}

// Do 方法执行并返回其方法的结果，确保针对一个key在同一时间只有一次调用。如果有重复的
// 请求过来，重复请求的调用者将进行等待第一个调用者的结果返回，并得到相同的结果。shared变量
// 标识此次调用是否此次的结果在多个接受者之间进行了共享。
func (g *Group) Do(key string, validTime time.Duration, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {

	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
		g.t = make(map[string]int64)
	}
	if c, ok := g.m[key]; ok { // 检查call结果是否存在
		t, _ := g.t[key]
		now := time.Now().Unix()

		if t > now { //还未过期需要重新查找
			c.dups++
			g.mu.Unlock()
			c.wg.Wait()
			return c.val, c.err, true
		}
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	// 判断结果，对时间进行赋值
	g.t[key] = getValidTime(validTime)
	g.mu.Unlock()

	g.doCall(c, key, fn)

	return c.val, c.err, c.dups > 0
}

// DoChan 像Do方法，但是不同的是返回一个通道。通道将把结果进行返回。
func (g *Group) DoChan(key string, validTime time.Duration, fn func() (interface{}, error)) <-chan Result {
	ch := make(chan Result, 1)
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
		g.t = make(map[string]int64)
	}
	if c, ok := g.m[key]; ok {
		t, _ := g.t[key]
		now := time.Now().Unix()

		if t > now { //还未过期需要重新查找
			c.dups++
			c.chans = append(c.chans, ch)
			g.mu.Unlock()
			return ch
		}
	}
	c := &call{chans: []chan<- Result{ch}}
	c.wg.Add(1)
	g.m[key] = c
	g.t[key] = getValidTime(validTime)
	g.mu.Unlock()

	go g.doCall(c, key, fn)

	return ch
}

// doCall 底层方法调用逻辑
func (g *Group) doCall(c *call, key string, fn func() (interface{}, error)) {
	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	if !c.forgotten {
		delete(g.m, key)
		delete(g.t, key)
	}
	for _, ch := range c.chans {
		ch <- Result{c.val, c.err, c.dups > 0}
	}
	g.mu.Unlock()
}

// Forget 方法告诉单飞去遗忘掉一个key。将来对Do方法的调用将调用方法去拿结果，
// 而不是等待之前的结果。
func (g *Group) Forget(key string) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		c.forgotten = true
	}
	delete(g.m, key)
	delete(g.t, key)
	g.mu.Unlock()
}

// 根据配置的可以时间，获得最终有效时间。
func getValidTime(validTime time.Duration) int64 {
	var t int64
	if validTime == 0 {
		t = math.MaxInt64
	} else {
		t = time.Now().Unix() + int64(validTime/time.Second)
	}
	return t
}
