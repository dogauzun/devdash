# Decisions

One line per non-obvious choice, newest last.

- 2026-10-02: All packages live under `internal/`; devdash promises no public Go API.
- 2026-10-02: Module path `github.com/dogauzun/devdash`, pending the name check (DEV-20).
- 2026-10-02: Linux collector skips kernel threads by `PF_KTHREAD` in the stat flags and any process with an empty cmdline, so zombies are skipped too (DEV-9).
- 2026-10-02: Linux collector: any read error other than EACCES means the process exited and the row is dropped; EACCES on statm/cmdline/cwd marks the field unknown; without stat/status (hidepid) there is no row at all (DEV-9).
- 2026-10-02: Linux fd walk scans own-uid pids, or every pid when running as root; a listening socket shared after fork goes to the lowest pid holding it, usually the parent (DEV-9).
- 2026-10-02: Verified as uid 1000 against root processes (kernel 7.0.12-linuxkit, Docker Desktop VM): stat, status, cmdline and statm are readable, so pid/ppid/uid/name/start time, argv, CPU time and RSS are known; cwd, fd/ and environ fail with EACCES; root's listeners are visible in /proc/net/tcp with their uid but no owner pid. Spec table holds (DEV-10).
- 2026-10-02: hidepid=2 and hidepid=1 both hide other users' processes from uid 1000 (stat unreadable or dir absent), while /proc/net/tcp still lists their listeners with uid; since kernel 5.8 /proc/mounts prints them as `hidepid=invisible` / `hidepid=noaccess`, so detection warns on any hidepid value other than 0/off (DEV-10).
- 2026-10-02: Linux snapshot p50 with 505 processes (450 sleep, 50 nc listeners, 1 root listener forcing a full fd walk), measured in Docker Desktop's linuxkit VM on an arm64 Mac, not bare metal: proctable 9.7 ms (target 25), argv_cwd 3.8 ms (15), listeners 5.0 ms (15), total 18.7 ms (60, budget 100) (DEV-11).
- 2026-10-02: Same setup plus 20 processes holding 500 fds each (~12.7k fds): listeners 18.9 ms (target 15), total 34 ms; fd walk costs ~1.4 µs per fd, so it scales with own-uid fds, not processes. Within budget, so no optimisation yet; first mitigation if needed is to re-check last tick's (pid, fd) per inode before a full walk (DEV-11).
- 2026-10-02: Linux collector skips zombies by stat state `Z`/`X` and keeps user processes with an empty cmdline (Argv nil), so a process that blanks its argv still owns its listeners; supersedes the empty-cmdline skip above (DEV-9 review).
- 2026-10-02: Linux collector reads `btime` once per collector lifetime, because the kernel shifts it when the wall clock is stepped and every StartTime would jump for one tick (DEV-9 review).
- 2026-10-02: Linux collector reads each pid in one pass (stat, status, statm, cmdline, cwd) so a pid reused between passes cannot mix two processes; per-source timings are summed per pid (DEV-9 review).
- 2026-10-02: For Phase 1: a `/proc/[pid]/cmdline` read can block on the target's mmap_lock, and ctx is only checked between reads, so the engine must run `Collect` in a goroutine and stop waiting after the 1.5 s timeout (DEV-9 review).
