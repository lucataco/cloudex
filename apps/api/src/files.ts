// Files through the API (FileInfo in @cloudex/schema): read, write, list and
// delete single files, and tar.gz archives of directories, as the cloudex user
// unless root is asked for, so the sandbox's own permissions apply. Contents
// stream through exec's standard input and output, so they're never held in
// memory; the size limit is Cloudflare's for a request body (100 MB on most
// plans), which only uploads meet.
//
// Paths may be relative to the user's home directory, or start with ~;
// answers carry absolute ones.

// Exit codes FILE_SCRIPT uses for errors meant for the caller.
export const FILE_NOT_FOUND = 3;
export const FILE_BAD_REQUEST = 4;
export const FILE_FORBIDDEN = 13;

// $1 is the operation, $2 the path:
//   stat <path>                          the path's FileInfo, with a directory's entries
//   check <path> <file|any>              FileInfo of a path to read: a readable file, or anything
//   write <path> <mode or ''> <bytes or ''>  the contents on stdin, exactly bytes long if given;
//                                        prints the new file's FileInfo
//   delete <path> <recursive 0|1>        prints the FileInfo of what went
//   mkdir <path>                         for an archive upload; prints its FileInfo
export const FILE_SCRIPT = String.raw`
import json, os, shutil, stat, sys, tempfile

MAX_ENTRIES = 10000


def fail(code, message):
    print(message, file=sys.stderr)
    sys.exit(code)


def resolve(p):
    if not p:
        fail(4, "no path given")
    return os.path.normpath(os.path.join(os.path.expanduser("~"), os.path.expanduser(p)))


def kind(m):
    if stat.S_ISREG(m):
        return "file"
    if stat.S_ISDIR(m):
        return "dir"
    if stat.S_ISLNK(m):
        return "symlink"
    return "other"


def entry(p, st):
    e = {"type": kind(st.st_mode), "size": st.st_size, "mode": format(stat.S_IMODE(st.st_mode), "o"),
         "mtime": st.st_mtime_ns // 1_000_000}
    if stat.S_ISLNK(st.st_mode):
        try:
            e["target"] = os.readlink(p)
        except OSError:
            pass
    return e


def lstat(p):
    try:
        return os.lstat(p)
    except FileNotFoundError:
        fail(3, "no such file or directory: " + p)
    except NotADirectoryError:
        fail(3, "no such file or directory: " + p)
    except PermissionError:
        fail(13, "permission denied: " + p)


def info(p, listing):
    st = lstat(p)
    out = {"path": p, **entry(p, st)}
    if listing and stat.S_ISDIR(st.st_mode):
        try:
            names = sorted(os.listdir(p))
        except PermissionError:
            fail(13, "permission denied: " + p)
        out["truncated"] = len(names) > MAX_ENTRIES
        out["entries"] = []
        for n in names[:MAX_ENTRIES]:
            q = os.path.join(p, n)
            try:
                out["entries"].append({"name": n, **entry(q, os.lstat(q))})
            except OSError:
                pass
    return out


op, p = sys.argv[1], resolve(sys.argv[2])
if op == "stat":
    print(json.dumps(info(p, True), ensure_ascii=False))
elif op == "check":
    st = lstat(p)
    if sys.argv[3] == "file":
        try:
            st = os.stat(p)
        except OSError:
            fail(3, "no such file: " + p + " is a broken symlink")
        if stat.S_ISDIR(st.st_mode):
            fail(4, p + " is a directory: list it, or download it as an archive")
        if not stat.S_ISREG(st.st_mode):
            fail(4, p + " isn't a regular file")
        if not os.access(p, os.R_OK):
            fail(13, "permission denied: " + p)
    elif not os.access(p, os.R_OK):
        fail(13, "permission denied: " + p)
    print(json.dumps(info(p, False), ensure_ascii=False))
elif op == "write":
    # Writes through a symlink to its target, and replaces the file whole, so
    # a reader never sees half of it.
    target = os.path.realpath(p)
    if os.path.isdir(target):
        fail(4, p + " is a directory")
    d = os.path.dirname(target)
    try:
        os.makedirs(d, exist_ok=True)
        fd, tmp = tempfile.mkstemp(dir=d, prefix=".cloudex-")
    except PermissionError:
        fail(13, "permission denied: " + d)
    except (FileExistsError, NotADirectoryError):
        fail(4, "can't make the directory " + d + ": something in its path is a file")
    try:
        with os.fdopen(fd, "wb") as f:
            shutil.copyfileobj(sys.stdin.buffer, f, 1 << 20)
            # An upload cut off on its way in looks like a shorter file.
            if sys.argv[4] and f.tell() != int(sys.argv[4]):
                fail(4, "the upload ended after %d of %s bytes, so %s wasn't changed" % (f.tell(), sys.argv[4], p))
            if sys.argv[3]:
                mode = int(sys.argv[3], 8)
            else:
                try:
                    mode = stat.S_IMODE(os.stat(target).st_mode)
                except FileNotFoundError:
                    mode = 0o666 & ~os.umask(0o022)
            os.fchmod(f.fileno(), mode)
        os.replace(tmp, target)
    except BaseException:
        os.unlink(tmp)
        raise
    print(json.dumps(info(target, False), ensure_ascii=False))
elif op == "delete":
    if p == "/" or p == os.path.expanduser("~"):
        fail(4, "won't delete " + p)
    st = lstat(p)
    out = info(p, False)
    try:
        if stat.S_ISDIR(st.st_mode):
            if sys.argv[3] != "1":
                fail(4, p + " is a directory: pass recursive to delete it and everything in it")
            shutil.rmtree(p)
        else:
            os.unlink(p)
    except PermissionError as e:
        fail(13, "permission denied: " + (e.filename or p))
    print(json.dumps(out, ensure_ascii=False))
elif op == "mkdir":
    try:
        os.makedirs(p, exist_ok=True)
    except PermissionError:
        fail(13, "permission denied: " + p)
    except (FileExistsError, NotADirectoryError):
        fail(4, p + " isn't a directory")
    if not os.path.isdir(p):
        fail(4, p + " isn't a directory")
    print(json.dumps(info(p, False), ensure_ascii=False))
else:
    fail(2, "unknown operation " + op)
`;

// $1 is a path that check found; packs it as tar.gz on stdout: a directory's
// contents, or the one file. Files it can't read are left out.
export const ARCHIVE_SCRIPT = String.raw`
set -o pipefail
if [ -d "$1" ] && [ ! -L "$1" ]; then
  tar -C "$1" --ignore-failed-read --warning=no-failed-read -czf - .
else
  tar -C "$(dirname -- "$1")" --ignore-failed-read --warning=no-failed-read -czf - -- "$(basename -- "$1")"
fi
`;

// $1 is a directory that mkdir made; unpacks a tar.gz from stdin into it, the
// files owned by whoever runs it. Prints how many entries it unpacked.
export const UNPACK_SCRIPT = String.raw`
set -euo pipefail
n=$(tar -C "$1" --no-same-owner -xzvf - | wc -l)
echo "$n"
`;
