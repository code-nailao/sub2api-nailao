package main

import (
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/processrole"
)

type cleanupPhase int

const (
	// cleanupBackgroundServices stops the global runners and schedulers (every
	// step outside localOrder in provideCleanup).
	cleanupBackgroundServices cleanupPhase = iota
	// cleanupRemaining stops the request-path services in localOrder, then
	// Redis and Ent.
	cleanupRemaining
)

// applicationCleanup runs provideCleanup's two phases at most once each and
// always in order. Serving and standalone only call Run, which is the same
// sequence as before the split. A background owner calls StopBackground while
// its listener still answers admin reads.
type applicationCleanup struct {
	run        func(cleanupPhase)
	background sync.Once
	remaining  sync.Once
}

func newApplicationCleanup(run func(cleanupPhase)) *applicationCleanup {
	return &applicationCleanup{run: run}
}

func (c *applicationCleanup) StopBackground() {
	c.background.Do(func() {
		started := time.Now()
		c.run(cleanupBackgroundServices)
		log.Printf("[Cleanup] Background services stopped in %s", time.Since(started))
	})
}

func (c *applicationCleanup) Run() {
	c.StopBackground()
	c.remaining.Do(func() { c.run(cleanupRemaining) })
}

// sourcesShutdown is the order in which Main stops after SIGINT or SIGTERM.
// Serving and standalone close the listener first, as before. The background
// owner keeps answering admin reads until its background services have
// stopped, so a release only loses the admin API for the new owner's start-up.
// Ownership is unaffected: the lock is released by process exit, after all of
// this has returned.
type sourcesShutdown struct {
	role           processrole.Role
	drain          func()
	waitWrites     func()
	stopBackground func()
	closeUpgrades  func()
	shutdownHTTP   func()
	waitIdle       func()
	exited         func()
	stopSettings   func()
	cleanup        func()
}

func (s sourcesShutdown) run() {
	s.drain()
	if s.role == processrole.Background {
		// Writes admitted before Drain finish before any service stops, as
		// they did when Shutdown came first; new ones are already refused.
		s.waitWrites()
		s.stopBackground()
		s.closeUpgrades()
	}
	s.shutdownHTTP()
	s.waitIdle()
	s.exited()
	s.stopSettings()
	s.cleanup()
}
