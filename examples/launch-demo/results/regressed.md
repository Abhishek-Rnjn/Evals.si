# recorded-regression

- **records:** 2
- **dataset sha256:** 1146d25ca6bc3f5d28efac9e93730abcc3efa5b748fd65541e46aab80199c8a0
- **evaluators:** builtin/exact-match@1.0.0
- **trials:** 1
- **started:** 2026-10-23T06:22:58.761357+00:00
- **finished:** 2026-10-23T06:22:58.761687+00:00
- **evalsi:** 0.3.1

## Metrics

| metric | n | mean | CI | skipped | errors |
|---|---:|---:|---|---:|---:|
| exact-match | 2 | 0.500 | [0.095, 0.905] | 0 | 0 |

## Gates

- ❌ exact-match mean ≥ 1.0 (mean 0.5 is below 1)
- ✅ exact-match error_rate ≤ 0.0

## Lowest exact-match

### japan (trial 0): fail
**input**

```
Capital of Japan?
```
**output**

```
Kyoto
```
**reference**

```
Tokyo
```
