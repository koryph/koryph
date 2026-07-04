# Changelog

## [0.5.0](https://github.com/koryph/koryph/compare/v0.4.0...v0.5.0) (2026-07-04)


### Features

* **ci:** branch-protection rulesets as code + ensure script ([b3e85b5](https://github.com/koryph/koryph/commit/b3e85b5c761aacc372a79adb4c038932c35278c4))
* **ci:** repo administrative settings as code + make repo-check/repo-apply ([17ec817](https://github.com/koryph/koryph/commit/17ec817ee0af293a3df6aee90b02a51e69dfcb9b))
* **cli:** add koryph release setup command ([962d0a7](https://github.com/koryph/koryph/commit/962d0a79af55e8d5f9e83cca39101a6849d2ff0d))
* **doctor:** add release-infra per-project checks ([e586dc2](https://github.com/koryph/koryph/commit/e586dc25b302f424a79843e1eee054f3294dce14))
* **project:** add release block to koryph.project.json schema ([031b848](https://github.com/koryph/koryph/commit/031b848bd7c2c82d3f5cb955b2fbcf92fe3388d1))
* **release:** add internal/release package with embedded templates ([b49d152](https://github.com/koryph/koryph/commit/b49d152f2bd34573c34ec73c089339dd21f92b69))
* **scripts:** provision-release-bot.sh — App Manifest bootstrap + zero-click repo attach ([9186734](https://github.com/koryph/koryph/commit/9186734a10b46e26954842a101680fc0da2cfea6))


### Bug Fixes

* **release:** grant contents:write to the slsa caller job ([b37cf21](https://github.com/koryph/koryph/commit/b37cf21590333b09b4f19fe133a92b4d13d6a13a))
* **release:** move tag+release ownership to GoReleaser, draft until complete ([901003c](https://github.com/koryph/koryph/commit/901003ce5395c25ec7e067fc82f11ac04ce419d5))

## [0.4.0](https://github.com/koryph/koryph/compare/v0.3.0...v0.4.0) (2026-07-04)


### Features

* **ci:** add ext-build/ext-test Makefile targets + VS Code extension CI job ([1b0b6e9](https://github.com/koryph/koryph/commit/1b0b6e9c74eba1e8d8733c44a415a033127c0ba2))
* **cli,sched:** koryph plan audit — corpus conflict analysis ([73ec96c](https://github.com/koryph/koryph/commit/73ec96c29eee974bc8e3ada1bf20cf6d65dbd15e))
* **cli:** accept --project on positional-id commands; improve not-found hint ([6df6690](https://github.com/koryph/koryph/commit/6df66908da118754751c470fa49334c74dba87fb))
* **cli:** add --json to governor show, signing status, project list ([4e882be](https://github.com/koryph/koryph/commit/4e882be8b3d5ab06317cf1bc94b1c8887a38acb3))
* **cli:** add koryph drain and koryph resize commands ([37c6cb2](https://github.com/koryph/koryph/commit/37c6cb2f693c4d97c59c5a595f838bcbaf85f8cd))
* **cli:** add roster command — per-bead lifecycle view for a run ([fb969af](https://github.com/koryph/koryph/commit/fb969afdbe94a4e8a4fe588047dbc8f4205d257f))
* **cli:** bash and zsh shell completions ([bd9a505](https://github.com/koryph/koryph/commit/bd9a505635f49337d0ec790e4be0d864b45bfb1e))
* **cli:** overhaul help discovery across the koryph CLI ([66b6171](https://github.com/koryph/koryph/commit/66b6171948171e9bb4595047e3e418d86e91a93e))
* **cli:** section the usage listing; add project install-assets grouped verb ([bd9499e](https://github.com/koryph/koryph/commit/bd9499ef34c4a508de1ac90f2af973aa795866ef))
* **commands:** add koryph-import slash command for markdown onboarding ([0504805](https://github.com/koryph/koryph/commit/0504805bc76298ae35df68a62627879505c3537e))
* **commands:** add koryph-ops.md — loop lifecycle ops template ([085cd54](https://github.com/koryph/koryph/commit/085cd547c6d684b523e8e024d604cec946185d06))
* **commands:** add koryph-plan template — design to conflict-aware bead graph ([dbbbea4](https://github.com/koryph/koryph/commit/dbbbea4a881aab8d686469571ba441f656068b28))
* **commands:** add koryph-replan slash command for corpus re-classification ([6a71e89](https://github.com/koryph/koryph/commit/6a71e893e188197d452d2f6f3fc7dec8ca36f26b))
* **dispatch:** classify rate-limit/overload markers in stream-json (koryph-2im.4) ([b3ab8fe](https://github.com/koryph/koryph/commit/b3ab8feed2af7591c93e42b0778a3da415092c62))
* **doctor:** asset drift detection + registry-wide refresh ([de4befd](https://github.com/koryph/koryph/commit/de4befddbf6bc488a36051e93d92ead90d4d85d2))
* **doctor:** integration matrix + suggestions engine ([a13eaa5](https://github.com/koryph/koryph/commit/a13eaa559691530fd62122f7d1d3f4d31e6c3e2c))
* **engine,cmd:** wire the AIMD overlay into dispatch, requeue, CLI, doctor (koryph-2im.4) ([d47f446](https://github.com/koryph/koryph/commit/d47f446819f919950b89982785cfa5384816f233))
* **engine,project,ledger,cmd:** rolling continuous-dispatch loop behind dispatch_mode ([a49f2bd](https://github.com/koryph/koryph/commit/a49f2bd4a0664e0321ac2d502b3a082c74d8650a))
* **engine,quota:** capability-gated quota advisory + registry.AccountFor wiring ([0c8dc24](https://github.com/koryph/koryph/commit/0c8dc24a61b57effeeef89995b7a7c05e6a5783c))
* **engine,quota:** fail-closed runtime dispatch gate + table-driven pricing (koryph-v8u.3) ([629eae6](https://github.com/koryph/koryph/commit/629eae6e3d6feeb9ae000e44c392ef6e06c5227a))
* **engine:** add inline PR line comments to koryph review-pr ([f3460ee](https://github.com/koryph/koryph/commit/f3460ee332fc5bcb1b94ffb1205b422075136cc5))
* **engine:** add koryph review-pr --all queue loop ([4627c9c](https://github.com/koryph/koryph/commit/4627c9c726927eca43eeccac01a3c7dd96fbf638))
* **engine:** add koryph review-pr for human-in-the-loop PR review ([6ca99f2](https://github.com/koryph/koryph/commit/6ca99f209766f1c078172f4ece7a8a2ea4bc8026))
* **engine:** add koryph run --direct owner override ([40c2e9f](https://github.com/koryph/koryph/commit/40c2e9f5e48c3a5524cc410f0085fb53bf89d9fa))
* **engine:** detect PRs closed/merged by any means ([43d609d](https://github.com/koryph/koryph/commit/43d609db68333ac8a500a50d3254862b5fca4c76))
* **engine:** fast completion detection — 10s poll, SIGCHLD wake, split probes ([c187b34](https://github.com/koryph/koryph/commit/c187b3476af480a04bfd11a863bc4e7a2564f7ae))
* **engine:** honor operator drain and resize at every wave/refill boundary ([739033c](https://github.com/koryph/koryph/commit/739033cf8e62aa3510bcbf1148714d9651b96018))
* **engine:** land engine-opened PRs fast-forward-only ([3af4429](https://github.com/koryph/koryph/commit/3af44297f78389286bca2c23d0eb2dbc265ec25d))
* **engine:** open a PR for merge_policy pr instead of parking the bead ([2b18a59](https://github.com/koryph/koryph/commit/2b18a59ed5e3052125cae948df8a5c09d6a626df))
* **engine:** raise gate/merge requeue budget to 2 via counters (koryph-2im.6) ([6f30142](https://github.com/koryph/koryph/commit/6f30142c9f66d5cf5044bca7c03f52c0d9b09e99))
* **engine:** review-pr IDE handoff (resume) + close ([3eef18e](https://github.com/koryph/koryph/commit/3eef18e1dcd2f413cccfe0f97407d3adb9838a79))
* **engine:** rolling dispatch becomes the default mode ([f686e6b](https://github.com/koryph/koryph/commit/f686e6bf9d811ae59e2a0658924126071c8c60a0))
* **engine:** surface wave skip/deferral reasons (were computed then discarded) ([1b9d621](https://github.com/koryph/koryph/commit/1b9d6215ea5b8ffe3e75d91d00f5ce1ba3966647))
* **engine:** wire in-flight footprint gating into wave build (koryph-2im.1) ([41499ce](https://github.com/koryph/koryph/commit/41499ce244a9793046e044a8fe6fc55327cf16d1))
* **govern:** AIMD overlay for the machine-wide concurrency cap (koryph-2im.4) ([d11af45](https://github.com/koryph/koryph/commit/d11af450103e4bc8ea3a7a2aca433f910b1f4d27))
* **govern:** per-provider governor pools (koryph-v8u.11, L5c) ([883eb33](https://github.com/koryph/koryph/commit/883eb3329250abba8e009c1d7293ffb79677069f))
* **govern:** raise default global agent cap 4-&gt;8 ([5bde58b](https://github.com/koryph/koryph/commit/5bde58bdb69470934398eaa60cd59a29ab3ab063))
* **govern:** settle windows, circuit breaker, dispatch smoothing (koryph-2im.11) ([c5b6585](https://github.com/koryph/koryph/commit/c5b6585e055f05c99f90bc514fe6333d2cbe9d20))
* **ide/vscode:** scaffold extension + read-only data layer ([d804984](https://github.com/koryph/koryph/commit/d804984f8eaf9ab1fb1dff45e0ad5cfcaeb08389))
* **ide/vscode:** slot commands — stop/nudge/model/worktree/diff/merge/land/PR ([60d910b](https://github.com/koryph/koryph/commit/60d910bc1d6448f473fef23508fc61fc122bb07f))
* **ide:** config editing UX — jsonValidation, edit command, run-start caveat banner ([6d0eda8](https://github.com/koryph/koryph/commit/6d0eda862498b45f25e4965683431e43c249a784))
* **ide:** tree view + quota status bar for agent threads ([59ea28c](https://github.com/koryph/koryph/commit/59ea28cf88605c500dd35c7a31f3428f7651a423))
* initial public release of koryph ([58db8c6](https://github.com/koryph/koryph/commit/58db8c6a1e7499d7c27b9796314c7d3709449c99))
* **ledger:** add operator drain sentinel and resize override store ([7189841](https://github.com/koryph/koryph/commit/7189841a25515e1995132ae84d73c22f6d4006de))
* **merge:** enforce conventional commits at merge/PR time ([9fa1e26](https://github.com/koryph/koryph/commit/9fa1e262cb55cd0a3fa9d149827d04f577b97155))
* **modelroute:** runtime-namespaced routing (koryph-v8u.3) ([6cc2c7e](https://github.com/koryph/koryph/commit/6cc2c7e3d6828395028f012d243ab72e0f3d55dd))
* **modelroute:** wire persona tier precedence into model resolution ([bb3f735](https://github.com/koryph/koryph/commit/bb3f735f4c4fd958dd48f25148a07bc6afbe2330))
* **personas:** pin plan-scorer to opus/xhigh + scheduler-correctness rubric ([8a403a8](https://github.com/koryph/koryph/commit/8a403a8dc858d8ab72e752b901ce75418fceb47e))
* **personas:** runtime-agnostic capability tiers (frontier/standard/light) ([fc5dd3d](https://github.com/koryph/koryph/commit/fc5dd3d736f0156ca3d6869d1fa6fc9491b96926))
* **project:** default_runtime + runtimes{} config block (koryph-v8u.3) ([8ad538d](https://github.com/koryph/koryph/commit/8ad538dedcf53e137a752458b67de0c10640037f))
* **project:** generate JSON Schema for koryph.project.json ([5804292](https://github.com/koryph/koryph/commit/5804292a1bfa43781b9430ea3845a88d16a8b0f1))
* **quota,personas:** per-runtime installer rendering + estimator bases ([76f39a2](https://github.com/koryph/koryph/commit/76f39a211048470f1dd2c7a3f65a6f3914eb2191))
* **quota:** document 40s ccusage latency, full-shape JSON test, and docs example ([baab3f8](https://github.com/koryph/koryph/commit/baab3f83ca9775685aa5be5c175dec87d6d9323d))
* **release:** chain release-please into GoReleaser, retire release.yml ([13c36ea](https://github.com/koryph/koryph/commit/13c36eafee844bf8be62b918dfbc957dc2a2d831))
* **runtime:** add claude adapter, the first real Runtime implementation ([b37d5df](https://github.com/koryph/koryph/commit/b37d5df2f2f708f5be7ef06ec30cac127a03eeff))
* **runtime:** add pluggable agent-runtime interface (koryph-v8u.1) ([bc0a156](https://github.com/koryph/koryph/commit/bc0a15614ec5bc371346803fd7a3ad277672709b))
* **runtime:** add tier-to-model map to the Runtime interface ([b3ae6eb](https://github.com/koryph/koryph/commit/b3ae6ebecf00f6afaa2e761bf554cbe752fb7e6f))
* **runtime:** add VerifyIdentity + UsageSource capability, registry runtime_accounts ([64c48da](https://github.com/koryph/koryph/commit/64c48daf976f1e79187774b73474f96e292f4d65))
* **sched:** RW footprint model + in-flight gating (koryph-2im.1) ([4dff596](https://github.com/koryph/koryph/commit/4dff5966a84c6e067fdfa91d1737583fd0287d6f))
* **security:** contain dispatched agents with an env allowlist + scoped signing agent ([0a51c52](https://github.com/koryph/koryph/commit/0a51c529af8668a5d3674f99eaa2bde16ac5bf3f))


### Bug Fixes

* **ci:** annotate the generated CHANGELOG.md in REUSE.toml ([7a2d682](https://github.com/koryph/koryph/commit/7a2d682db8848258e2d4f0e66bca4988ff260083))
* **ci:** cover ide/vscode/media in REUSE.toml; stop gate swallowing lint failures ([225d0ec](https://github.com/koryph/koryph/commit/225d0ec08353864ec88e4cb495b9f80266c7efa5))
* **ci:** REUSE compliance + de-flake orphan reconciliation test ([1c3dbb8](https://github.com/koryph/koryph/commit/1c3dbb87b4c083d30cb0f494aa4ba5830f2a61ea))
* **ci:** trailing newline in .beads/config.yaml ([48c6072](https://github.com/koryph/koryph/commit/48c60724012c6f21ace30c554720a8338b6af3b4))
* **commands:** koryph-plan — runtime-neutral tiers + exact RW conflict semantics ([9f84cf4](https://github.com/koryph/koryph/commit/9f84cf4a51f52fabb853eb76a3cf95ea07f0310c))
* **engine:** thread bead id into rate-limit reports ([0d168b1](https://github.com/koryph/koryph/commit/0d168b17d3071d3b308bd6ebef04ceef80926151))
* **metrics:** drop unconditional JSON line from Render ([8173b55](https://github.com/koryph/koryph/commit/8173b552286ea560e8bf6db46b26732cec51c07f))
* **quota:** flock-guard per-account config against lost updates ([e9ec21a](https://github.com/koryph/koryph/commit/e9ec21a0100c55ddf61cec07b1cf70629dc8d298))
* **sched:** footprint labels compose — fp:* no longer suppresses area:* writes ([0fb1626](https://github.com/koryph/koryph/commit/0fb16267e9875bbcbde882d6187c158c9bd440e7))
* **security:** harden merge DefaultProtected + case-insensitive matching ([ae7fb80](https://github.com/koryph/koryph/commit/ae7fb80ecb8c049b62d039329d7842d1538e1ca2))
* **security:** install guard hooks outside the agent-writable worktree ([262f06c](https://github.com/koryph/koryph/commit/262f06c67fb793332a5447bd6e9ff44d3e831895))
* **security:** screen git config persistence vectors in agent-boundary-guard ([3728dae](https://github.com/koryph/koryph/commit/3728dae6d3820c5d8d2fa8803220ad6a837107c7))
* **security:** tighten ~/.koryph tree to 0700 + private state files to 0600 ([9f78c8c](https://github.com/koryph/koryph/commit/9f78c8cb36f927c41ab5a24a58c4651e2abb1829))


### Performance Improvements

* **engine:** batch per-tick ledger writes; skip unchanged checkpoints ([a46e5a6](https://github.com/koryph/koryph/commit/a46e5a60fcdbe14544b416058f0c0122c332b5bd))
