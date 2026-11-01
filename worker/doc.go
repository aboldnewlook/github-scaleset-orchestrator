// Package worker is the worker side of gso.worker.v1 (design spec
// docs/design/remote-workers.md §4, §8). It parses join strings, pins the
// router's key, verifies every JIT config before launch, stores the worker
// credential and its runner records, and (client.go) runs the session with the
// router.
//
// The package is public so the Burst desktop app can import it. It must not
// import gso's internal packages, cobra, viper, bubbletea or actions/scaleset;
// runner/deps_test.go enforces this.
package worker
