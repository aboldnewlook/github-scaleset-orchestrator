# Changelog

## [0.6.0](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.5.3...v0.6.0) (2026-04-02)


### Features

* add goreleaser config for cross-platform binary releases ([#28](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/28)) ([04db5b7](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/04db5b74df0a8e424a41e89170c3388c19a9c173))
* **tui:** add event filtering by repo/type and completed jobs count ([#23](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/23)) ([34c9311](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/34c9311241bce6af1e510f50ecd40391f6b9bb73))
* **tui:** add event search and detail expansion ([#26](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/26)) ([5c1fbf7](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/5c1fbf7f2d2b8e036c4e27d1612a28a6f9295310))
* **tui:** add URL open hotkey and OSC 8 clickable links ([#29](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/29)) ([54d16fa](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/54d16fa1ad4d27534112c3500533f699739b1590))


### Bug Fixes

* **tui:** format JSON numbers as integers to avoid scientific notation in URLs ([#30](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/30)) ([95e9531](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/95e95315b79c6e07c3f31d2b247042e4900b0367))
* **tui:** freeze timer for done runners and exclude from active count ([#22](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/22)) ([8de07d6](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/8de07d6ffbddd8dcc8b63a541326083ddf9b152e))
* **tui:** prevent same-timestamp events from being dropped ([#27](https://github.com/aboldnewlook/github-scaleset-orchestrator/issues/27)) ([8597331](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/859733145452c053503d6e8fe17a76cde5a412af))

## [0.5.3](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.5.2...v0.5.3) (2026-03-31)


### Bug Fixes

* create .config/gso/tls directory and set XDG_CONFIG_HOME in Dockerfile ([729a659](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/729a659bfd4703ab6676c06d21fd763b824b01b4))

## [0.5.2](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.5.1...v0.5.2) (2026-03-31)


### Bug Fixes

* pass TLS client options to TUI model for remote connections ([4203529](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/4203529299725177b41d7390e6c77ab273f45b13))

## [0.5.1](https://github.com/aboldnewlook/github-scaleset-orchestrator/compare/v0.5.0...v0.5.1) (2026-03-31)


### Bug Fixes

* ensure version and build info are included in release image ([d2b6829](https://github.com/aboldnewlook/github-scaleset-orchestrator/commit/d2b6829b02eb9274f9a1bcaa8c40f6032c1fb80c))

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
