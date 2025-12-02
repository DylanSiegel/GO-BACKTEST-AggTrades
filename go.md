Title: Go 1.25.4 / Windows 11 / Ryzen 9 7900X – Environment & Coding Rules for AI Code Generation

-------------------------------------------------------------------------------
1. PURPOSE AND POINT OF VIEW
-------------------------------------------------------------------------------

This document defines the *actual* development environment and coding rules
for an AI that generates Go code.

It assumes the AI may have outdated or partial knowledge of modern Go and
standard library additions (Go 1.23–1.25), and corrects that by:

- Pinning the exact Go version and OS/CPU target.
- Describing which experimental features are ENABLED in this environment.
- Defining which standard library packages and idioms to PREFER.
- Stating hard constraints (pure stdlib, no external dependencies).

The AI should treat everything in this document as ground truth for this
specific environment, even if its internal training data disagrees.


-------------------------------------------------------------------------------
2. TARGET ENVIRONMENT (HARDWARE + SOFTWARE)
-------------------------------------------------------------------------------

Hardware
- CPU: AMD Ryzen 9 7900X
  - Architecture: Zen 4
  - Cores/Threads: 12 cores / 24 threads
  - L3 Cache: 64 MB
  - ISA: x86-64 with AVX-512 and BMI2
- RAM: 32 GB DDR5

OS / Toolchain
- OS: Windows 11
- Go: 1.25.4
- Target: GOOS=windows, GOARCH=amd64 (i.e. `windows/amd64`)

Hard Constraints
- NO CGO. All code must be pure Go, using only the standard library.
- NO external modules or third-party packages.
- NO vendor trees.
- Everything must build and run with:
  - `go1.25.4 build ./...`
  - on Windows 11, `windows/amd64`.


-------------------------------------------------------------------------------
3. GLOBAL ENVIRONMENT SETTINGS (ASSUMED ACTIVE)
-------------------------------------------------------------------------------

Unless stated otherwise, assume the following environment variables are set
on the host machine:

1) Instruction Set Level

- `GOAMD64=v4`

Meaning:
- The Go toolchain is allowed to assume a “level 4” x86-64 CPU.
- On this specific CPU (Zen 4), AVX-512 and BMI2 are available.
- The compiler and runtime may use more advanced instructions in assembly
  and runtime/stdlib internals where implemented.

Important nuance:
- This does *not* mean that every tight loop is magically autovectorized to
  512-bit SIMD in pure Go. It *does* mean that:
  - Architecture-specific code paths in runtime/stdlib can take advantage
    of the richer ISA level.
  - We are not targeting older CPUs; code generation can assume a modern
    AMD64 platform.

2) Garbage Collector and JSON Experiments

We assume the following experiments are enabled by default for this environment:

- `GOEXPERIMENT=greenteagc,jsonv2`

Implications:

- `greenteagc`
  - Enables the experimental “Green Tea” garbage collector in Go 1.25.
  - Intended to improve GC performance, locality, and scalability, especially
    for workloads with many small objects.
  - It is *experimental*: design and behavior may evolve in future Go versions.
  - For this environment, treat it as the default GC.

- `jsonv2`
  - Enables the experimental `encoding/json/v2` and `encoding/json/jsontext`
    packages, and causes `encoding/json` to run on the v2 implementation.
  - APIs and semantics may change in future Go releases, but are considered
    stable *enough* for this environment.

The AI should:
- Assume both experiments are on for code that targets this environment.
- Still write code that degrades gracefully (i.e., does not *require*
  experiments for correctness — only for performance/behavior improvements).

3) GC Target Heap Size

- `GOGC=200` (default tuning)

Meaning:
- The garbage collector is allowed to grow the heap more aggressively, reducing
  GC frequency at the expense of additional memory usage.
- This is a *starting point* for throughput-oriented workloads on a 32 GB
  system, not a universal constant. Real tuning still depends on profiling.


-------------------------------------------------------------------------------
4. GO VERSION AND LANGUAGE-LEVEL ASSUMPTIONS
-------------------------------------------------------------------------------

Version
- All code is written for Go 1.25.4.
- Newer features introduced in Go 1.23, 1.24, and 1.25 are available and
  may be relied upon, specifically:
  - `unique` package (Go 1.23+).
  - `iter` package and iterator APIs in stdlib (Go 1.23+).
  - `weak` package (Go 1.24+).
  - Experimental JSON v2 stack (`encoding/json/v2`, `encoding/json/jsontext`)
    when `GOEXPERIMENT=jsonv2` is set.
  - Experimental “Green Tea” GC when `GOEXPERIMENT=greenteagc` is set.

Language Features
- Generics are fully available and should be used where appropriate.
- Typed `sync/atomic` types are available and preferred over raw `Uint64`
  with manual atomic operations.
- Range-iteration over functions and `iter.Seq` are available.


-------------------------------------------------------------------------------
5. HARD CONSTRAINTS FOR THE AI
-------------------------------------------------------------------------------

When generating code for this environment, the AI MUST:

1) Use only the standard library:
   - No references to external modules (no `go get`, no `require` in go.mod
     beyond the main module).
   - No cgo imports, no `import "C"`.

2) Target `windows/amd64`:
   - Paths: use backslashes in examples where OS-specific paths are needed,
     but prefer using `filepath.Join` instead of hard-coded separators.
   - Do not rely on Unix-only syscalls or paths.

3) Assume `GOAMD64=v4`:
   - No need to support older x86-64 levels.
   - But never hard-code assembly; rely on stdlib/runtime to exploit ISA.

4) Be conservative with *undocumented* internals:
   - Do not rely on specific SIMD instruction usage or undocumented GC
     details, even if inferred from benchmarks or blog posts.
   - Treat low-level tuning as “allowed but opaque”: we benefit from it,
     but we do not depend on its exact shape.


-------------------------------------------------------------------------------
6. PREFERRED STANDARD LIBRARY FEATURES
-------------------------------------------------------------------------------

6.1 Iterators: `iter`, `slices`, `maps`

- `iter` is available and integrated into the standard library.
- `iter.Seq[T]` and related types should be used when:
  - Building composable data-processing pipelines.
  - Interacting with `slices` or other stdlib APIs that accept/return `iter.Seq`.

Guidelines:
- Use iterator-based patterns for:
  - Pipelines where composition and lazy evaluation are valuable.
  - APIs that naturally present themselves as sequences (streaming, traversal).
- Use plain `for` loops for:
  - The hottest inner loops over slices/arrays when raw throughput is critical.
  - Low-level operations where iterator abstraction would add overhead.

Example pattern:

```go
import "iter"

func FilterEven(seq iter.Seq[int]) iter.Seq[int] {
    return func(yield func(int) bool) {
        for v := range seq {
            if v%2 == 0 {
                if !yield(v) {
                    return
                }
            }
        }
    }
}
````

6.2 Interning: `unique`

* `unique` is used to canonicalize comparable values (e.g. strings).
* Primary use case: high-cardinality string keys such as:

  * User IDs, event types, metric names, log keys, JSON field names.

Guidelines:

* Use `unique.Handle[string]` (or other types) when:

  * Many duplicate values are expected.
  * Identity or equality checks are frequent.
* Avoid interning:

  * Huge or very low-reuse values.
  * Values where deduplication offers no real memory or performance benefit.

Example:

```go
import "unique"

type UserID = unique.Handle[string]

func CanonicalUserID(raw string) UserID {
    return unique.Make(raw)
}
```

6.3 Weak References: `weak`

* `weak` provides weak pointers that do not keep objects alive.
* Typical use: caches, memoization, or internal maps that must not prevent
  garbage collection of values.

Guidelines:

* Use `weak.Pointer[T]` for:

  * Caches whose entries are expendable under memory pressure.
  * Structures that reference large objects but should not prolong their
    lifetime unnecessarily.
* Combine `weak` with a higher `GOGC` and `unique` for aggressive memory
  optimizations:

  * Intern many keys with `unique`.
  * Use `weak.Pointer` in caches to avoid unbounded growth.

Example:

```go
import "weak"

type Big struct {
    // ...
}

var cache = make(map[string]weak.Pointer[Big])
```

6.4 Concurrency: `sync/atomic` Typed Values

* Prefer typed atomic values for simple shared state over mutexes:

  * `atomic.Int64`, `atomic.Uint64`, `atomic.Bool`, `atomic.Pointer[T]`, etc.

Guidelines:

* Use `sync/atomic` when:

  * You need a single counter, flag, or pointer modified from multiple goroutines.
* Use `sync.Mutex` / `sync.RWMutex` when:

  * You must protect compound invariants involving multiple fields or complex data.

Example:

```go
import "sync/atomic"

var activeRequests atomic.Int64

func incr() {
    activeRequests.Add(1)
}

func get() int64 {
    return activeRequests.Load()
}
```

6.5 JSON: `encoding/json` and `encoding/json/v2`

Given `GOEXPERIMENT=jsonv2` is enabled:

* `encoding/json/v2` and `encoding/json/jsontext` are available.
* `encoding/json` is backed by the v2 implementation.

Guidelines:

* Default:

  * Use `encoding/json` for typical marshaling/unmarshaling needs.
* Use `encoding/json/v2` when:

  * You want explicit control over v2-specific APIs and behaviors.
* Use `encoding/json/jsontext` for:

  * Lower-level streaming/token-based JSON I/O where fine-grained control
    and performance are critical.

Important:

* These APIs are *experimental* and not guaranteed stable across Go versions.
* In this environment, they are considered acceptable for use.
* Do not encode assumptions about internal SIMD/BMI2 usage; treat performance
  characteristics as empirical, not guaranteed.

---

7. PERFORMANCE PHILOSOPHY FOR THIS ENVIRONMENT

---

The AI should optimize with the following priorities:

1. Correctness first.

   * No reliance on undefined behavior or undocumented runtime invariants.

2. Throughput over minimal binary size.

   * A slightly larger binary is acceptable if it significantly improves
     CPU throughput or reduces allocations.

3. Favor low-allocation patterns:

   * Reuse buffers via `sync.Pool` *only when justified* by profiling.
   * Avoid unnecessary allocations in tight loops (e.g. avoid converting
     slices to `[]interface{}`).

4. Exploit parallelism:

   * Use goroutines and channels *when* they add net benefit on 12C/24T,
     but avoid over-sharding tasks that will just contend on locks or GC.

5. Leverage stdlib primitives designed for performance:

   * `bytes`, `strings`, `sync/atomic`, `unique`, `weak`, `iter`, `slices`.
   * Prefer these over ad-hoc reimplementations.

6. Measure, don’t assume:

   * The environment is tuned for performance (GOAMD64=v4, greenteagc, jsonv2,
     GOGC=200), but real tuning still requires benchmarks and profiling.
   * Generated code should be structured in a way that is easy to benchmark
     and profile (e.g. public functions with clear inputs/outputs).

---

8. WINDOWS-SPECIFIC ASSUMPTIONS

---

* Target is Windows 11; code must:

  * Use `os` and `syscall` only in ways supported on Windows.
  * Avoid Unix-only assumptions (e.g. `/proc`, Unix sockets, hard-coded `/tmp`).
* File paths:

  * When building paths, prefer `filepath.Join`.
  * Do not hard-code `/` or `\` unless absolutely necessary for examples.

---

9. ENVIRONMENT SETUP SNIPPET (POWERSHELL)

---

The following shows how this environment is expected to be configured on
Windows 11, using per-user environment variables:

```powershell
# Instruction set level: assume modern x86-64 with AVX-512, BMI2.
[System.Environment]::SetEnvironmentVariable(
    "GOAMD64", "v4",
    [System.EnvironmentVariableTarget]::User
)

# GC target: allow a larger heap for fewer collections.
[System.Environment]::SetEnvironmentVariable(
    "GOGC", "200",
    [System.EnvironmentVariableTarget]::User
)

# Enable experimental Green Tea GC and JSON v2.
[System.Environment]::SetEnvironmentVariable(
    "GOEXPERIMENT", "greenteagc,jsonv2",
    [System.EnvironmentVariableTarget]::User
)
```

---

10. CHECKLIST FOR THE AI WHEN GENERATING CODE

---

For any Go code you generate in this environment, ensure that:

1. It builds with:

   * Go 1.25.4
   * `GOOS=windows`, `GOARCH=amd64`
   * `GOAMD64=v4`
   * `GOEXPERIMENT=greenteagc,jsonv2`

2. It uses only the standard library, with no cgo or external modules.

3. It treats the following as available and encouraged where appropriate:

   * `iter` for iterator-based APIs and pipelines.
   * `unique` for interning high-cardinality comparable values.
   * `weak` for caches and other structures that should not keep objects alive.
   * Typed `sync/atomic` values for simple shared state.
   * `encoding/json` (backed by v2), and optionally `encoding/json/v2` and
     `encoding/json/jsontext` for advanced JSON usage.

4. It does NOT rely on:

   * Pre-1.23 limitations (e.g. “Go has no iterators,” “no weak pointers”).
   * Specific low-level assembly or SIMD instruction choices.
   * Behavior that is explicitly described as experimental beyond what is
     stated here.

5. It favors:

   * Clear, idiomatic Go using modern stdlib features.
   * High-throughput, reasonably low-allocation patterns aligned with a
     12C/24T, AVX-512-capable CPU and 32 GB RAM.
