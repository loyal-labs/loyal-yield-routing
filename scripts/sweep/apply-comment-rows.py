#!/usr/bin/env python3
"""Apply comment-sweep #3 row decisions.

delete <rows.tsv> [--skipped <path>]
    Delete the comment blocks of every DELETE row in the sweep TSV. A row is
    matched by its normalized quote of the block's first line: the stated
    line first, then a +-5-line window, then the whole file when that match
    is unique. Skipped rows (unmatched, ambiguous, protected) are printed
    with their reason, and with --skipped also written to that file.

strip-tails <paths...>
    Inside Go // comments, remove parentheticals whose content is only
    TypeScript line refs and drop comment lines whose whole text is such a
    ref list. Prose is never reflowed.

Both passes are rerunnable: a second run changes nothing and says so.
Stdlib only; run as `python3 -I scripts/sweep/apply-comment-rows.py ...`.
"""

import csv
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

# Owner holds: no row may delete into these ranges (1-indexed, inclusive).
# S1 owns deposit_context.go:115-160, R12 owns privy_provisioning.go:60-75.
PROTECTED = {
    'go/workers/internal/webapp/deposit_context.go': [(115, 160)],
    'go/workers/internal/webapp/privy_provisioning.go': [(60, 75)],
}

# Directive comments that tooling parses; never delete one.
DIRECTIVE_PREFIXES = ('//go:', '//nolint', '// +build', '//export', '// @ts-', '// eslint')

# A TypeScript line reference: name.ts:12, name.tsx:12-34, pkg/file.ts:12.
TS_REF = r'[\w./~-]+\.(?:tsx?):\d+(?:[-–]\d+)?'
TS_LIST = rf'{TS_REF}(?:\s*,\s*{TS_REF})*'
# A parenthetical holding only refs: (route.ts:35-78), (a.ts:1, b.tsx:2-3).
TS_PAREN = re.compile(rf' ?\(({TS_LIST})\)')
# A comment whose whole text is a ref list: // route.ts:257-281
TS_BARE = re.compile(rf'^\(?\s*{TS_LIST}\s*\)?\s*\.?$')


def norm(s):
    return re.sub(r'[^a-z0-9]', '', s.lower())


def quote_key(raw):
    """Normalize a row's quote field to a match key (verify.py's idea).

    Trailing annotations after the closing quote — ` (block 1–7; ...)`,
    ` (both consts)`, ` etc.` — are not part of the quoted line and are cut.
    """
    s = raw.replace('""', '"').replace('\\|', '|').strip()
    s = s.strip('`" ').strip()
    s = re.sub(r'^//\s*', '', s)
    s = re.split(r' \((?:block |both |whole )', s)[0]
    s = re.sub(r'\s+etc\.$', '', s)
    return norm(s)


def parse_ref(ref):
    """'file.go:22' or 'file.go:368,374,380 (8)' -> (name, [368, 374, ...])."""
    m = re.match(r'\s*`?([^:`]+):([\d,\s]+)', ref or '')
    if not m:
        return None, []
    return m.group(1).strip(), [int(x) for x in re.findall(r'\d+', m.group(2))]


def resolve_path(name, check):
    """Prefer the check column's resolved path; fall back to a suffix match."""
    parts = check.split(' ', 1)
    if len(parts) == 2 and parts[0] in ('OK', 'MISS') and parts[1].strip():
        p = parts[1].strip()
        if os.path.isfile(os.path.join(ROOT, p)):
            return p
    hits = []
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = [d for d in dirnames if d not in ('.git', 'node_modules', '.next', 'dist')]
        for fn in filenames:
            if fn == name:
                hits.append(os.path.relpath(os.path.join(dirpath, fn), ROOT))
    exact = [h for h in hits if h.endswith('/' + name)] or hits
    return exact[0] if len(exact) == 1 else None


def scan_comment(line, in_raw):
    """Index of the // starting a line comment, honoring Go string literals.

    Returns (index or -1, in_raw) where in_raw carries a multi-line raw
    string across calls.
    """
    i, n = 0, len(line)
    while i < n:
        c = line[i]
        if in_raw:
            if c == '`':
                in_raw = False
            i += 1
            continue
        if c == '`':
            in_raw = True
        elif c == '"' or c == "'":
            i += 1
            while i < n and line[i] != c:
                i += 2 if line[i] == '\\' else 1
        elif c == '/' and i + 1 < n and line[i + 1] == '/':
            return i, in_raw
        i += 1
    return -1, in_raw


def comment_text(line, in_raw=0):
    """The comment text of a line (after //), or None; carries raw-string state."""
    idx, in_raw = scan_comment(line, in_raw)
    if idx < 0:
        return None, in_raw
    return line[idx + 2:], in_raw


def is_pure_comment(line):
    return line.lstrip().startswith('//')


def is_directive(line):
    return line.lstrip().startswith(DIRECTIVE_PREFIXES)


def leading_ws(line):
    return line[:len(line) - len(line.lstrip())]


def is_protected(path, lineno):
    return any(a <= lineno <= b for a, b in PROTECTED.get(path, []))


def load_lines(rel):
    with open(os.path.join(ROOT, rel), encoding='utf-8') as f:
        return f.read().split('\n')


def line_matches(text, key):
    """A candidate matches when the normalized quote is a prefix of its
    normalized text. Prefix-only: containment let rows whose block was
    already deleted capture lookalike lines on a rerun."""
    t = norm(text)
    if not key:
        return False
    return t.startswith(key)


def find_anchor(lines, path, stated, key, claimed):
    """Index of the row's comment line: stated line, +-5 window, whole file."""
    def hit(i):
        if i < 0 or i >= len(lines) or i in claimed:
            return False
        if is_protected(path, i + 1):
            return False
        text, _ = comment_text(lines[i])
        if text is None:
            return False
        # A row quoting a trailing comment includes the code before the //.
        if line_matches(text, key) or line_matches(lines[i], key):
            return True
        # A quote may span the block's first two comment lines.
        if is_pure_comment(lines[i]) and i + 1 < len(lines) and is_pure_comment(lines[i + 1]):
            text2, _ = comment_text(lines[i + 1])
            return line_matches(text + ' ' + (text2 or ''), key)
        return False

    exact = stated - 1
    if hit(exact):
        return exact, None
    window = [i for i in range(max(0, stated - 6), min(len(lines), stated + 5)) if hit(i)]
    if len(window) == 1:
        return window[0], None
    if len(window) > 1:
        window.sort(key=lambda i: (abs(i + 1 - stated), i))
        if abs(window[0] + 1 - stated) < abs(window[1] + 1 - stated):
            return window[0], None
        return None, f'ambiguous: {len(window)} window lines match the quote'
    in_file = [i for i in range(len(lines)) if hit(i)]
    if len(in_file) == 1:
        return in_file[0], None
    if len(in_file) > 1:
        return None, f'ambiguous: {len(in_file)} lines in the file match the quote'
    return None, 'no comment line matches the quote'


def find_subline(lines, path, stated, key, claimed):
    """A member line of a multi-line row: a pure comment line near the stated
    number that cites a TS ref (or matches the quote)."""
    for i in range(max(0, stated - 4), min(len(lines), stated + 3)):
        if i in claimed or not is_pure_comment(lines[i]) or is_directive(lines[i]):
            continue
        if is_protected(path, i + 1):
            continue
        text, _ = comment_text(lines[i])
        if text is None:
            continue
        if re.search(TS_REF, text) or line_matches(text, key):
            return i
    return None


def block_end(lines, path, idx, claimed):
    """Last index of the comment block starting at idx: following pure
    comment lines at the same indentation, stopping at a blank line, code,
    a directive, any other row's first line, or a protected range."""
    first = lines[idx]
    if not is_pure_comment(first):
        return idx  # trailing comment: only the comment itself goes
    end = idx
    lead = leading_ws(first)
    in_block = first.lstrip().startswith('/*') and '*/' not in first
    j = idx + 1
    while j < len(lines):
        line = lines[j]
        s = line.strip()
        if s == '' or is_directive(line) or j in claimed or is_protected(path, j + 1):
            break
        if not (line.lstrip().startswith('//') or line.lstrip().startswith('*')):
            break
        if leading_ws(line) != lead:
            break
        if line.lstrip().startswith('/*'):
            break
        end = j
        if in_block and '*/' in line:
            break
        j += 1
    return end


def apply_delete(rows):
    """Delete comment blocks for every DELETE row; returns (applied, skips)."""
    targets = []  # dicts: path, lines, key, row, only_line
    skips = []    # (row, reason)

    for row in rows:
        if len(row) < 8 or row[3].strip() != 'DELETE':
            continue
        name, lns = parse_ref(row[1])
        key = quote_key(row[2])
        only = re.search(r'only line (\d+)', row[2])
        if not name or not lns:
            skips.append((row, 'row has no file:line reference'))
            continue
        path = resolve_path(name, row[7])
        if not path:
            skips.append((row, f'file {name!r} not found on disk'))
            continue
        targets.append({'path': path, 'lines': lns, 'key': key, 'row': row,
                        'only_line': int(only.group(1)) if only else None})

    by_path = {}
    for t in targets:
        by_path.setdefault(t['path'], []).append(t)

    changed_files = set()
    applied = 0

    for path, group in sorted(by_path.items()):
        lines = load_lines(path)
        # A quote shared by several rows of one file disables the whole-file
        # search tier for those rows: a rerun must never steal a sibling.
        key_counts = {}
        for t in group:
            key_counts[t['key'][:20]] = key_counts.get(t['key'][:20], 0) + 1

        claims = {}  # line index -> target
        for t in sorted(group, key=lambda t: t['lines'][0]):
            idx, why = find_anchor(lines, path, t['lines'][0], t['key'], claims)
            if idx is None:
                if key_counts[t['key'][:20]] > 1 and why and 'file match' in why:
                    why = 'duplicate quote text in file; whole-file search disabled'
                skips.append((t['row'], why or 'no match'))
                continue
            claims[idx] = t
            owned = [idx]
            misses = []
            for ln in t['lines'][1:]:
                sub = find_subline(lines, path, ln, t['key'], claims)
                if sub is None:
                    misses.append(ln)
                else:
                    claims[sub] = t
                    owned.append(sub)
            if misses:
                print(f"note: {path} row at :{t['lines'][0]} skipped members "
                      f"{','.join(str(m) for m in misses)} (no comment line)")
            applied += 1

        # Block ranges on the original lines, then delete bottom-up.
        drops = set()
        edits = {}  # index -> new line (trailing comment stripped)
        for idx in sorted(claims):
            t = claims[idx]
            if t.get('only_line') is not None:
                end = idx
            else:
                end = block_end(lines, path, idx, claims)
            line = lines[idx]
            if is_pure_comment(line):
                drops.update(range(idx, end + 1))
            else:
                cut, _ = scan_comment(line, 0)
                if cut >= 0:
                    edits[idx] = line[:cut].rstrip()

        if not drops and not edits:
            continue
        new_lines = []
        for i, line in enumerate(lines):
            if i in edits:
                new_lines.append(edits[i])
            elif i in drops:
                continue
            else:
                new_lines.append(line)
        # A deleted block above `package` is a file header: no leading blank.
        pkg = next((i for i, l in enumerate(lines) if l.startswith('package ')), None)
        if pkg is not None and any(d < pkg for d in drops):
            while new_lines and new_lines[0].strip() == '':
                new_lines.pop(0)
        # Collapse double blanks gofmt would reject; all predate the edit.
        collapsed, out = False, []
        for line in new_lines:
            if line.strip() == '' and out and out[-1].strip() == '':
                collapsed = True
                continue
            out.append(line)
        new = '\n'.join(out)
        old = '\n'.join(lines)
        if new != old:
            with open(os.path.join(ROOT, path), 'w', encoding='utf-8') as f:
                f.write(new)
            changed_files.add(path)
            removed = len(lines) - len(out)
            print(f'{path}: -{removed} line(s), {len(edits)} trailing comment(s) stripped'
                  if edits else f'{path}: -{removed} line(s)')

    return applied, skips, sorted(changed_files)


def strip_go_tails(path, lines):
    """Strip TS-ref parentheticals and bare-ref comment lines from one file."""
    changed = 0
    out = []
    in_raw = 0
    for line in lines:
        text, in_raw = comment_text(line, in_raw)
        new_line = line
        if text is not None and not is_directive(line):
            body = text.strip()
            cut, _ = scan_comment(line, 0)
            code = line[:cut]
            if TS_BARE.fullmatch(body):
                new_line = code.rstrip() if code.strip() else None
            else:
                stripped = TS_PAREN.sub('', text)
                if stripped != text:
                    new_line = code + '//' + stripped.rstrip()
        if new_line is None:
            changed += 1
            continue
        if new_line != line:
            changed += 1
        out.append(new_line)
    return out, changed


def apply_strip(paths):
    total_files, total_lines = 0, 0
    for p in paths:
        rel = os.path.relpath(os.path.abspath(p), ROOT) if os.path.isabs(p) else p
        if os.path.isdir(rel):
            got = apply_strip([os.path.join(rel, f) for f in sorted(os.listdir(rel))
                               if not f.startswith('.')])
            total_files += got[0]
            total_lines += got[1]
            continue
        if not rel.endswith('.go') or not os.path.isfile(rel):
            continue
        lines = load_lines(rel)
        new_lines, changed = strip_go_tails(rel, lines)
        if changed:
            with open(os.path.join(ROOT, rel), 'w', encoding='utf-8') as f:
                f.write('\n'.join(new_lines))
            print(f'{rel}: {changed} line(s) changed')
            total_files += 1
            total_lines += changed
    return total_files, total_lines


def parse_args(argv):
    """Split `--skipped <path>` out of argv; the rest stays positional."""
    skipped = None
    rest = []
    i = 0
    while i < len(argv):
        if argv[i] == '--skipped':
            if i + 1 >= len(argv):
                print('--skipped needs a path argument', file=sys.stderr)
                return None, None
            skipped = argv[i + 1]
            i += 2
        else:
            rest.append(argv[i])
            i += 1
    return skipped, rest


def main(argv):
    skipped, args = parse_args(argv)
    if args is None or len(args) < 2 or args[0] not in ('delete', 'strip-tails'):
        print(__doc__.strip(), file=sys.stderr)
        return 2
    if args[0] == 'delete':
        with open(args[1], newline='', encoding='utf-8') as f:
            rows = [r for r in csv.reader(f, delimiter='\t') if r and r[1].strip()]
        applied, skips, changed = apply_delete(rows)
        if skipped:
            with open(skipped, 'w', newline='', encoding='utf-8') as f:
                w = csv.writer(f, delimiter='\t', lineterminator='\n')
                w.writerow(['slice', 'ref', 'quote', 'verdict', 'kind', 'fix', 'owner', 'check', 'reason'])
                for row, why in skips:
                    w.writerow(row[:8] + [why])
        print(f'applied {applied} skipped {len(skips)} ({len(changed)} files changed)')
        for row, why in skips:
            print(f"  skip {row[1][:48]}: {why}")
        if applied == 0 and not changed:
            print('no changes: every DELETE row is already applied or unmatched')
        return 0

    files, lines = apply_strip(args[1:])
    print(f'stripped {lines} line(s) across {files} file(s)')
    if lines == 0:
        print('no changes: no TypeScript ref tails left')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
