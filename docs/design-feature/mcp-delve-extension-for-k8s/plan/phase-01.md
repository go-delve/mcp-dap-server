---
parent: ./README.md
phase: 1
name: Fork + CI verification
estimate: 0.5 day
depends_on: none
---

# Phase 1: Fork + CI verification

## Scope

Форк `github.com/go-delve/mcp-dap-server → github.com/vajrock/mcp-dap-server-k8s-forward` уже создан (см. текущий working directory). Этот этап — только **верификация**: убедиться, что CI существующего репо работает без изменений кода, и подготовить инфраструктуру для последующих фаз (test-run + docker-available для integration).

## Files Affected

### Check / verify (no changes)
- `.github/workflows/go.yml` — существующий CI workflow из upstream. Проверить, что:
  - Trigger: push/PR на master
  - Steps: `go mod download` → `go build -v ./...` → `go test -v ./...`
  - Go version: 1.26.1 (matching `go.mod`)
- `.goreleaser.yaml` — упомянут в `CLAUDE.md`, нужен для release workflow; не трогаем в Phase 1.
- `Dockerfile.minimal`, `Dockerfile.debug` — существуют; не трогаем.
- `go.mod`, `go.sum` — не трогаем.

### No new files in Phase 1.

## Implementation Steps

1. **Проверка локальной сборки** — убедиться, что на текущем branch (`master`, commit `422a5a8`) проект собирается и тесты проходят:
   ```bash
   go build -v ./...
   go test -v -race ./...
   ```
   Если что-то не проходит — это upstream-проблема, фиксим отдельным коммитом перед началом Phase 2. Ожидаемо — всё проходит (есть fresh upstream-коммит).

2. **Проверка GitHub Actions** — открыть `.github/workflows/go.yml`, убедиться, что workflow референсится из `master` и не требует секретов, которых нет (при форке часть секретов могла пропасть).

3. **Docker availability для integration tests** — проверить локально, что `docker compose` доступен (нужен начиная с Phase 7). Не требует изменений репо, но фиксируем ssh-шпаргалку в README плана если отсутствует.

4. **Создать ветку для всего эпика** (`feat/mcp-k8s-remote`) — все последующие фазы коммитятся туда. Merge в master — после approval и всех фаз.
   ```bash
   git checkout -b feat/mcp-k8s-remote
   ```

5. **(Опционально) Создать draft-PR на GitHub** для ранней visibility — статус "Draft", описание "In progress, see design docs in `docs/design-feature/mcp-delve-extension-for-k8s/`". Это чисто organizational, не обязательно.

## Success Criteria

- [ ] `go build -v ./...` проходит на чистом clone
- [ ] `go test -v -race ./...` — все upstream-тесты зелёные, race detector clean
- [ ] GitHub Actions CI на master **последний run успешен** (если не — это blocker, fix upstream-compat перед Phase 2)
- [ ] Branch `feat/mcp-k8s-remote` создан и checkout'ен
- [ ] `docker compose version` возвращает валидную версию

## Tests to Pass

- Все существующие upstream-тесты (`tools_test.go`, если есть backend-тесты, etc.).
- Никаких новых тестов не добавляется в этой фазе.

## Risks

- **CI broken после форка** (missing secrets) — fixable один коммит правок в `.github/workflows/go.yml`.
- **Несовместимая версия Go** (upstream 1.26.1, локально старше) — install Go 1.26+ или использовать Docker-based builder.

## Output / Deliverable

Одна branch `feat/mcp-k8s-remote`, основанная на `master` (commit `422a5a8`), с зелёным CI и локальным `go test` passing. Ничего нового не закоммичено.
