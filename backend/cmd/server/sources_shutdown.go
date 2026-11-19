package main

import (
	"context"
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
	started    time.Time
}

func newApplicationCleanup(run func(cleanupPhase)) *applicationCleanup {
	return &applicationCleanup{run: run}
}

func (c *applicationCleanup) StopBackground() {
	c.background.Do(func() {
		c.started = time.Now()
		c.run(cleanupBackgroundServices)
		log.Printf("[Cleanup] Background services stopped in %s", time.Since(c.started))
	})
}

func (c *applicationCleanup) Run() {
	c.StopBackground()
	c.remaining.Do(func() {
		c.run(cleanupRemaining)
		log.Printf("[Cleanup] All cleanup steps completed in %s", time.Since(c.started))
	})
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
		// Requests a draining owner refuses (writes, MCP, reads that probe
		// upstream or write account state) that were admitted before Drain
		// finish before any service stops, as they did when Shutdown came
		// first; new ones are already refused.
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

// newSourcesShutdown wires the sequence to the live runtime and application.
// Kept apart from main so a test can check the wiring, not only the order.
func newSourcesShutdown(runtime *processrole.Runtime, app *Application, stopSettings func()) sourcesShutdown {
	return sourcesShutdown{
		role:  runtime.Role(),
		drain: runtime.Drain,
		waitWrites: func() {
			if err := runtime.WaitAdminWrites(context.Background()); err != nil {
				log.Printf("Admin write drain: %v", err)
			}
		},
		stopBackground: app.Cleanup.StopBackground,
		closeUpgrades:  runtime.CloseAdminUpgrades,
		// A release removes this generation from routing before TERM. Keep active
		// streams alive; the operator must never escalate a slow drain to SIGKILL.
		shutdownHTTP: func() {
			if err := app.Server.Shutdown(context.Background()); err != nil {
				log.Printf("Server shutdown: %v", err)
			}
		},
		// net/http does not wait for native 202 image tasks or hijacked connections.
		// Their result persistence must finish before native queues/clients close.
		waitIdle: func() {
			if err := runtime.Wait(context.Background()); err != nil {
				log.Printf("Instance drain: %v", err)
			}
		},
		exited:       func() { log.Println("Server exited") },
		stopSettings: stopSettings,
		cleanup:      app.Cleanup.Run,
	}
}
