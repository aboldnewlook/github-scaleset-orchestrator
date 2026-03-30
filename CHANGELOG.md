# Changelog

## [0.5.0](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.4.0...v0.5.0) (2026-03-30)


### Features

* add build-time version injection and version command ([6be8293](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/6be829305b39a74e2b89bc11effee76224228dbf))
* security hardening, auto-TLS, TOFU, and IP allowlist ([e57aaeb](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/e57aaeb83687029c73313aaaec7b4e626be20d6b))


### Bug Fixes

* **ci:** add build-essential to runtime image for CGO support ([5e9b3e2](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/5e9b3e248fcc472325b60850f28ef3378c0a222f))

## [0.4.0](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.3.0...v0.4.0) (2026-03-30)


### Features

* stream events over TCP and add runner linger display ([f03fb54](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/f03fb549d184423392d1de008625666da454ac4b))

## [0.3.0](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.2.1...v0.3.0) (2026-03-30)


### Features

* enable TCP listener by default, add auth and connection logging ([f8c1766](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/f8c1766a18f20e2d1eeeafdfa3c392c8937c5f2e))

## [0.2.1](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.2.0...v0.2.1) (2026-03-30)


### Reverts

* switch back to ubuntu runtime image ([24dda98](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/24dda98a86e3c8f9f50538d38eb497249249a65a))

## [0.2.0](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.1.0...v0.2.0) (2026-03-28)


### Features

* add TCP support for remote control socket connections ([#2](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/2)) ([fff0849](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/fff084905f479a33b855c1c1bcf62ab5d6d0610f))
* add Viper config with env var support ([#1](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/1)) ([477858d](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/477858d2f537255a20590bd9d4566ff95cb9eb3f))
