# flux-diff

CLI-утилита для локальной сборки и сравнения (diff) Flux GitOps-ресурсов без подключения к живому кластеру.

## Запуск через Docker

### Сборка образа

```bash
docker build -t flux-diff .
```

### Базовые команды

Все команды выполняются через `docker run` с монтированием git-репозитория:

```bash
docker run --rm -v /path/to/your/repo:/repo -w /repo flux-diff <command>
```

#### Сборка Kustomization

```bash
docker run --rm -v $(pwd):/repo -w /repo flux-diff build ks --path clusters/prod/
```

#### Сборка HelmRelease

```bash
docker run --rm -v $(pwd):/repo -w /repo flux-diff build hr podinfo --path clusters/prod/
```

#### Diff Kustomization (сравнение с default branch)

```bash
docker run --rm -v $(pwd):/repo -w /repo flux-diff diff ks --path clusters/prod/
```

#### Diff HelmRelease (сравнение с указанной веткой)

```bash
docker run --rm -v $(pwd):/repo -w /repo flux-diff diff hr podinfo --path clusters/prod/ --branch main
```

### Коды возврата

| Код | Значение |
|-----|----------|
| 0   | Успех, различий не найдено |
| 1   | Найдены различия |
| 2   | Ошибка |

### Aliases

Для удобства можно создать shell-alias:

```bash
alias flux-diff='docker run --rm -v $(pwd):/repo -w /repo flux-diff'
```

После этого команды можно запускать напрямую:

```bash
flux-diff build ks --path clusters/prod/
flux-diff diff hr podinfo --path clusters/prod/
```
