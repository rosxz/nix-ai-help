#!/usr/bin/env python3
import argparse
import csv
import json
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Dict, Optional

from jinja2 import Template

HASH_RE = re.compile(r"[0-9a-f]{7,40}", re.IGNORECASE)
PROMPT_TOKENS_RE = re.compile(r"Prompt Tokens:\s*(\d+)")
CACHED_TOKENS_RE = re.compile(r"Cached Tokens:\s*(\d+)")
COMPLETION_TOKENS_RE = re.compile(r"Completion Tokens:\s*(\d+)")
TOTAL_COST_RE = re.compile(r"Total Cost:\s*\$?([0-9]+(?:\.[0-9]+)?)")


def parse_project_url(fetcher: str) -> Optional[str]:
    if not fetcher:
        return None

    url_match = re.search(r'url\s*=\s*"(https?://[^"]+)"', fetcher)
    if url_match:
        url = url_match.group(1)
        return normalize_project_url(url)

    owner_match = re.search(r'owner\s*=\s*"([^"]+)"', fetcher)
    repo_match = re.search(r'repo\s*=\s*"([^"]+)"', fetcher)
    if not owner_match or not repo_match:
        return None

    owner = owner_match.group(1).strip()
    repo = repo_match.group(1).strip()
    domain_match = re.search(r'domain\s*=\s*"([^"]+)"', fetcher)
    host_match = re.search(r'host\s*=\s*"([^"]+)"', fetcher)
    domain = None
    if domain_match:
        domain = domain_match.group(1).strip()
    elif host_match:
        domain = host_match.group(1).strip()

    if "fetchFromGitLab" in fetcher and not domain:
        domain = "gitlab.com"
    if "fetchFromGitHub" in fetcher and not domain:
        domain = "github.com"

    if not domain:
        return None

    return f"https://{domain}/{owner}/{repo}"


def normalize_project_url(url: str) -> Optional[str]:
    if not url:
        return None

    cleaned = url.strip()
    cleaned = cleaned.rstrip("/")
    if cleaned.endswith(".git"):
        cleaned = cleaned[:-4]

    if "github.com" in cleaned or "gitlab.com" in cleaned:
        parts = cleaned.split("/")
        try:
            idx = parts.index("github.com")
        except ValueError:
            try:
                idx = parts.index("gitlab.com")
            except ValueError:
                idx = None
        if idx is not None and len(parts) >= idx + 3:
            return "/".join(parts[: idx + 3])

    return cleaned


def slugify(value: str) -> str:
    value = value.strip()
    value = re.sub(r"[^A-Za-z0-9._-]+", "-", value)
    value = re.sub(r"-+", "-", value)
    return value.strip("-") or "package"


def find_nix_file(output_dir: Path) -> Optional[Path]:
    nix_files = sorted(p for p in output_dir.glob("*.nix") if p.name not in {"flake.nix", "package.nix"})
    if not nix_files:
        return None
    return nix_files[0]


def get_attr_pos(attr: str, cwd: Path) -> Optional[int]:
    """Get the line number of an attribute in a Nix file."""
    result = subprocess.run(
        [
            "nix",
            "eval",
            ".#default",
            "--impure",
            "--apply",
            f'pkg: (builtins.unsafeGetAttrPos "{attr}" pkg).line',
        ],
        cwd=str(cwd),
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"get_attr_pos: Failed to get attribute position: {result.stderr}")
    pos_str = result.stdout.strip()
    try:
        return int(pos_str)
    except ValueError:
        raise RuntimeError(f"get_attr_pos: Failed to parse attribute position: {pos_str}")


def rewrite_src_ref_attributes(
    file_path: Path,
    line_num: int,
    replacement: str,
    keys: tuple[str, ...],
    force_rev_key: bool = False,
) -> bool:
    """Rewrite matching src reference attributes and return whether any replacement was made."""
    with file_path.open("r+", encoding="utf-8") as handle:
        lines = handle.readlines()
        depth = 0
        started = False
        replaced = False
        key_pattern = "|".join(re.escape(k) for k in keys)

        for i in range(line_num - 1, len(lines)):
            depth += lines[i].count("{") - lines[i].count("}")
            if "{" in lines[i]:
                started = True

            match = re.match(rf"^(\s*)({key_pattern})\s*=.*", lines[i])
            if match:
                indent, key = match.groups()
                target_key = "rev" if force_rev_key else key
                lines[i] = f'{indent}{target_key} = "{replacement}";\n'
                replaced = True

            if started and depth <= 0:
                break

        if replaced:
            handle.seek(0)
            handle.truncate()
            handle.writelines(lines)

    return replaced


def replace_src_with_fetcher(file_path: Path, line_num: int, fetcher: str) -> bool:
    """Replace the src attribute with the provided fetcher expression."""
    fetcher_value = fetcher.strip()
    if not fetcher_value:
        return False

    with file_path.open("r+", encoding="utf-8") as handle:
        lines = handle.readlines()
        if line_num < 1 or line_num > len(lines):
            return False

        start_idx = line_num - 1
        indent_match = re.match(r"^(\s*)src\s*=", lines[start_idx])
        indent = indent_match.group(1) if indent_match else ""

        depth = 0
        started = False
        end_idx = start_idx
        for i in range(start_idx, len(lines)):
            if i == start_idx and "src" in lines[i]:
                started = True
            depth += lines[i].count("{") - lines[i].count("}")
            end_idx = i
            if started and depth <= 0 and ";" in lines[i]:
                break

        fetcher_lines = [line.rstrip() for line in fetcher_value.splitlines() if line.strip()]
        if not fetcher_lines:
            return False

        new_lines = [f"{indent}src = {fetcher_lines[0]}\n"]
        for line in fetcher_lines[1:]:
            new_lines.append(f"{indent}{line}\n")
        if new_lines[-1].rstrip().endswith(";"):
            pass
        else:
            new_lines[-1] = f"{new_lines[-1].rstrip()};\n"

        lines[start_idx : end_idx + 1] = new_lines
        handle.seek(0)
        handle.truncate()
        handle.writelines(lines)
        return True


def sanitize_fetcher(fetcher: str) -> str:
    """Remove finalAttrs. references from fetcher text."""
    return re.sub(r"\bfinalAttrs\.", "", fetcher)


def replace_attr_value(file_path: Path, line_num: int, attr: str, value: str) -> bool:
    """Replace a single-line attribute assignment with a quoted value."""
    if not value.strip():
        return False

    with file_path.open("r+", encoding="utf-8") as handle:
        lines = handle.readlines()
        if line_num < 1 or line_num > len(lines):
            return False

        idx = line_num - 1
        match = re.match(rf"^(\s*){re.escape(attr)}\s*=", lines[idx])
        if not match:
            return False
        indent = match.group(1)
        lines[idx] = f'{indent}{attr} = "{value}";\n'
        handle.seek(0)
        handle.truncate()
        handle.writelines(lines)
        return True


def run_command(
    args,
    cwd: Path,
    dry_run: bool,
    timeout: Optional[int] = None,
    env: Optional[Dict[str, str]] = None,
) -> tuple[int, bool]:
    if dry_run:
        print("DRY-RUN:", " ".join(args), "(cwd:", cwd, ")")
        return 0, False
    run_env = None
    if env is not None:
        run_env = dict(os.environ)
        run_env.update(env)
    try:
        result = subprocess.run(args, cwd=str(cwd), timeout=timeout, env=run_env)
        return result.returncode, False
    except subprocess.TimeoutExpired:
        return 1, True


def run_command_capture(
    args,
    cwd: Path,
    dry_run: bool,
    timeout: Optional[int] = None,
    env: Optional[Dict[str, str]] = None,
) -> tuple[int, bool, str, str]:
    if dry_run:
        print("DRY-RUN:", " ".join(args), "(cwd:", cwd, ")")
        return 0, False, "", ""
    run_env = None
    if env is not None:
        run_env = dict(os.environ)
        run_env.update(env)
    try:
        result = subprocess.run(
            args,
            cwd=str(cwd),
            timeout=timeout,
            env=run_env,
            capture_output=True,
            text=True,
        )
        if result.stdout:
            print(result.stdout, end="")
        if result.stderr:
            print(result.stderr, end="", file=sys.stderr)
        return result.returncode, False, result.stdout or "", result.stderr or ""
    except subprocess.TimeoutExpired:
        return 1, True, "", ""


def _last_int_match(pattern: re.Pattern[str], text: str) -> Optional[int]:
    matches = pattern.findall(text)
    if not matches:
        return None
    return int(matches[-1])


def _last_float_match(pattern: re.Pattern[str], text: str) -> Optional[float]:
    matches = pattern.findall(text)
    if not matches:
        return None
    return float(matches[-1])


def parse_nixai_usage(output: str) -> Optional[Dict[str, Any]]:
    if not output:
        return None
    prompt_tokens = _last_int_match(PROMPT_TOKENS_RE, output)
    cached_tokens = _last_int_match(CACHED_TOKENS_RE, output)
    completion_tokens = _last_int_match(COMPLETION_TOKENS_RE, output)
    total_cost = _last_float_match(TOTAL_COST_RE, output)
    if prompt_tokens is None or cached_tokens is None or completion_tokens is None or total_cost is None:
        return None
    return {
        "prompt_tokens": prompt_tokens,
        "cached_tokens": cached_tokens,
        "completion_tokens": completion_tokens,
        "total_cost": total_cost,
    }


def get_nixpkgs_lock_info(commit: str) -> Dict[str, Any]:
    """Generate flake lock information for a specific nixpkgs commit using nurl."""
    cmd = [
        "nurl",
        "--json",
        "https://github.com/nixos/nixpkgs",
        commit,
    ]

    result = subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        check=True,
    )

    fetcher_data = json.loads(result.stdout)
    nar_hash = fetcher_data["args"]["hash"]

    return {
        "lastModified": int(time.time()),
        "narHash": nar_hash,
        "rev": commit,
    }


def detect_package_set(row: Dict[str, Any]) -> Optional[str]:
    raw_name = (row.get("fully_qualified_name") or "").strip()
    if not raw_name or "." not in raw_name:
        return None

    parts = [part for part in raw_name.split(".") if part]
    if not parts:
        return None

    if parts[0] == "pkgs":
        parts = parts[1:]
    if len(parts) < 2:
        return None

    return ".".join(parts[:-1])


def init_git_repo(repo_dir: Path, dry_run: bool) -> None:
    if (repo_dir / ".git").exists():
        return
    if dry_run:
        print(f"DRY-RUN: init git repo in {repo_dir}")
        return

    try:
        import git
    except ImportError:
        result_code, _ = run_command(["git", "init"], cwd=repo_dir, dry_run=False)
        if result_code != 0:
            print(f"git init failed for {repo_dir}")
            return
        run_command(["git", "add", "-A"], cwd=repo_dir, dry_run=False)
        commit_cmd = [
            "git",
            "-c",
            "user.name=package-bot",
            "-c",
            "user.email=package-bot@example.invalid",
            "commit",
            "-m",
            "init package",
        ]
        result_code, _ = run_command(commit_cmd, cwd=repo_dir, dry_run=False)
        if result_code != 0:
            print(f"git commit failed for {repo_dir}")
        return

    author = git.Actor("package-bot", "package-bot@example.invalid")
    repo = git.Repo.init(repo_dir)
    repo.git.add("-A")
    if repo.is_dirty(untracked_files=True):
        repo.index.commit("init package", author=author, committer=author)


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate package repos from CSV rows.")
    parser.add_argument("csv_path", type=Path, help="Path to CSV file.")
    parser.add_argument("--packages-dir", type=Path, default=Path("packages"), help="Base output directory.")
    parser.add_argument(
        "--template-dir",
        type=Path,
        default=None,
        help="Directory containing flake.nix.template and flake.lock.j2.",
    )
    parser.add_argument("--repo-root", type=Path, default=None, help="Repo root containing flake.nix.")
    parser.add_argument(
        "--soft-limit",
        type=int,
        default=0,
        help="Limit number of packages that pass nixai (no timeout and exit code 0).",
    )
    parser.add_argument(
        "--hard-limit",
        type=int,
        default=0,
        help="Limit number of packages considered regardless of nixai timeout.",
    )
    parser.add_argument(
        "--start",
        type=int,
        default=0,
        help="Skip rows with random_order less than this value.",
    )
    parser.add_argument("--dry-run", action="store_true", help="Print actions without running commands.")
    parser.add_argument("--skip-existing", action="store_true", help="Skip if output directory exists.")
    parser.add_argument("--model", type=str, default="openai.gpt-oss-120b", help="Model to use for nixai (default: openai.gpt-oss-120b).")
    args = parser.parse_args()

    script_dir = Path(__file__).resolve().parent
    repo_root = args.repo_root or script_dir.parent
    template_dir = args.template_dir or script_dir
    flake_template_path = template_dir / "flake.nix.template"
    flake_lock_template_path = template_dir / "flake.lock.j2"
    flake_set_template_path = template_dir / "flake.nix.set.template"

    if not args.csv_path.exists():
        print(f"CSV not found: {args.csv_path}")
        return 1
    if not flake_template_path.exists():
        print(f"Template not found: {flake_template_path}")
        return 1
    if not flake_set_template_path.exists():
        print(f"Template not found: {flake_set_template_path}")
        return 1
    if not flake_lock_template_path.exists():
        print(f"Template not found: {flake_lock_template_path}")
        return 1

    base_output = args.packages_dir
    if not base_output.is_absolute():
        base_output = repo_root / base_output

    processed = 0
    hard_count = 0
    soft_count = 0
    stats = {
        "eval_failed": [],
        "regex_replace_failed": [],
        "build_failed": [],
        "build_succeeded": [],
        "nixai_timeout": [],
        "url_parse_failed": [],
    }
    usage_totals = {
        "prompt_tokens": 0,
        "cached_tokens": 0,
        "completion_tokens": 0,
        "total_cost": 0.0,
    }
    usage_by_package = []
    with args.csv_path.open("r", newline="", encoding="utf-8") as handle:
        reader = csv.DictReader(handle)
        rows = []
        for row in reader:
            raw_order = (row.get("random_order") or "").strip()
            if not raw_order:
                print("Skipping row: missing random_order")
                continue
            try:
                order_value = int(raw_order)
            except ValueError:
                print(f"Skipping row: invalid random_order '{raw_order}'")
                continue
            rows.append((order_value, row))

        rows.sort(key=lambda item: item[0])
        for _, (order_value, row) in enumerate(rows):
            if args.start and order_value < args.start:
                continue

            if args.hard_limit and hard_count >= args.hard_limit:
                break
            if args.soft_limit and soft_count >= args.soft_limit:
                break

            hard_count += 1

            print("==============")
            fetcher = (row.get("fetcher") or "").strip()
            fetcher = sanitize_fetcher(fetcher)
            project_url = parse_project_url(fetcher)
            if not project_url:
                print(f"Skipping row {idx}: could not parse project URL")
                stats["url_parse_failed"].append(str(idx))
                continue

            name = row.get("package_name") or row.get("pname") or row.get("fully_qualified_name") or "package"
            nixpkgs_commit = (row.get("nixpkgs_target_bump") or "").strip()
            if not nixpkgs_commit:
                print(f"Skipping {name}: missing nixpkgs_target_bump")
                continue
            output_dir = base_output / slugify(name)
            if args.skip_existing and output_dir.exists():
                print(f"Skipping {name}: output exists")
                continue

            output_dir.mkdir(parents=True, exist_ok=True)

            pkg_set = detect_package_set(row)

            existing_nix = (output_dir / "package.nix").exists() or any(output_dir.glob("*.nix"))
            if existing_nix:
                print(f"Skipping nixai for {name}: output directory already has Nix files")
            else:
                cmd = [
                    "nix",
                    "run",
                    ".#nixai",
                    "--",
                    "package-repo",
                    project_url,
                    "--provider",
                    "bedrock",
                    "--model",
                    args.model,
                    "--output",
                    str(output_dir),
                ]
                nixai_env = {
                    "GIT_TERMINAL_PROMPT": "0",
                    "NIXAI_PRINT_PACKAGING_PROMPT": "0",
                }
                result_code, timed_out, stdout, _ = run_command_capture(
                    cmd,
                    cwd=repo_root,
                    dry_run=args.dry_run,
                    timeout=15,
                    env=nixai_env,
                )
                if not args.dry_run:
                    usage = parse_nixai_usage(stdout)
                    if usage:
                        usage["package"] = name
                        usage_by_package.append(usage)
                        usage_totals["prompt_tokens"] += usage["prompt_tokens"]
                        usage_totals["cached_tokens"] += usage["cached_tokens"]
                        usage_totals["completion_tokens"] += usage["completion_tokens"]
                        usage_totals["total_cost"] += usage["total_cost"]
                        print(
                            "Cost summary for {name}: Prompt={prompt} Cached={cached} Completion={completion} Total=${cost:.8f}".format(
                                name=name,
                                prompt=usage["prompt_tokens"],
                                cached=usage["cached_tokens"],
                                completion=usage["completion_tokens"],
                                cost=usage["total_cost"],
                            )
                        )
                        costs_path = output_dir / "costs.txt"
                        costs_content = (
                            "Prompt Tokens: {prompt}\n"
                            "Cached Tokens: {cached}\n"
                            "Completion Tokens: {completion}\n"
                            "Total Cost: ${cost:.8f}\n"
                        ).format(
                            prompt=usage["prompt_tokens"],
                            cached=usage["cached_tokens"],
                            completion=usage["completion_tokens"],
                            cost=usage["total_cost"],
                        )
                        costs_path.write_text(costs_content, encoding="utf-8")
                    else:
                        print(f"No cost summary found for {name}")
                elif args.dry_run:
                    print(f"DRY-RUN: write costs.txt in {output_dir}")
                if timed_out:
                    print(f"nixai timed out for {name}")
                    stats["nixai_timeout"].append(str(output_dir))
                    continue
                if result_code != 0:
                    print(f"nixai failed for {name}")
                    continue

                soft_count += 1

                nix_file = find_nix_file(output_dir)
                if nix_file:
                    target = output_dir / "package.nix"
                    if args.dry_run:
                        print(f"DRY-RUN: rename {nix_file} -> {target}")
                    else:
                        if target.exists():
                            target.unlink()
                        nix_file.rename(target)
                else:
                    print(f"No .nix file found for {name}")
                    continue

                flake_target = output_dir / "flake.nix"
                flake_lock_target = output_dir / "flake.lock"
                if args.dry_run:
                    if pkg_set:
                        print(f"DRY-RUN: render {flake_set_template_path} -> {flake_target}")
                    else:
                        print(f"DRY-RUN: copy {flake_template_path} -> {flake_target}")
                    print(f"DRY-RUN: render {flake_lock_template_path} -> {flake_lock_target}")
                else:
                    if pkg_set:
                        flake_template = flake_set_template_path.read_text(encoding="utf-8")
                        flake_content = flake_template.replace("{{ pkg_set }}", pkg_set)
                        flake_target.write_text(flake_content, encoding="utf-8")
                    else:
                        shutil.copyfile(flake_template_path, flake_target)
                    with flake_lock_template_path.open("r", encoding="utf-8") as handle:
                        template = Template(handle.read())
                    lock_info = get_nixpkgs_lock_info(nixpkgs_commit)
                    flake_lock_content = template.render(**lock_info)
                    flake_lock_target.write_text(flake_lock_content, encoding="utf-8")

            init_git_repo(output_dir, args.dry_run)

            revision = (row.get("version") or "").strip()
            if not revision:
                print(f"No version for {name}, skipping nix-update")
                processed += 1
                continue

            package_nix = output_dir / "package.nix"
            try:
                src_line = get_attr_pos("src", cwd=output_dir)
            except RuntimeError as exc:
                print(f"Failed to find src position for {name}: {exc}")
                src_line = None

            try:
                version_line = get_attr_pos("version", cwd=output_dir)
            except RuntimeError as exc:
                print(f"Failed to find version position for {name}: {exc}")
                version_line = None

            src_replaced = False
            version_replaced = False
            if src_line:
                if args.dry_run:
                    print(f"DRY-RUN: replace src in {package_nix} using fetcher from CSV")
                    src_replaced = True
                else:
                    src_replaced = replace_src_with_fetcher(package_nix, src_line, fetcher)
                    if not src_replaced:
                        print(f"No src replacement applied for {name}")
            else:
                print(f"No src position found for {name}")

            if version_line:
                if args.dry_run:
                    print(f"DRY-RUN: replace version in {package_nix} using CSV value")
                    version_replaced = True
                else:
                    version_replaced = replace_attr_value(package_nix, version_line, "version", revision)
                    if not version_replaced:
                        print(f"No version replacement applied for {name}")
            else:
                print(f"No version position found for {name}")

            if not (src_replaced and version_replaced):
                stats["regex_replace_failed"].append(str(output_dir))

            print(f"Nix eval for {name}")
            eval_cmd = ["nix", "eval", ".#default"]
            result_code, _ = run_command(eval_cmd, cwd=output_dir, dry_run=args.dry_run)
            if result_code != 0:
                print(f"nix eval failed for {name}")
                stats["eval_failed"].append(str(output_dir))
                continue

            # print(f"nix-update for {name} with version {version_arg}")
            # update_cmd = ["nix-update", "default", "--flake", f"--version={version_arg}"]
            # if run_command(update_cmd, cwd=output_dir, dry_run=args.dry_run) != 0:
            #     print(f"nix-update failed for {name}")
            #     stats["update_failed"].append(str(output_dir))
            #     continue

            build_cmd = ["nix", "build", ".#default"]
            result_code, _ = run_command(build_cmd, cwd=output_dir, dry_run=args.dry_run)
            if result_code != 0:
                print(f"nix build failed for {name}")
                stats["build_failed"].append(str(output_dir))
                continue

            if args.dry_run:
                print(f"DRY-RUN: touch {output_dir / 'build.success'}")
            else:
                (output_dir / "build.success").touch()
            stats["build_succeeded"].append(str(output_dir))
            processed += 1

    print(f"Processed {processed} rows")
    print("Statistics:")
    print(f"  eval failed: {len(stats['eval_failed'])}")
    print(f"  regex replace failed: {len(stats['regex_replace_failed'])}")
    print(f"  build failed: {len(stats['build_failed'])}")
    print(f"  build succeeded: {len(stats['build_succeeded'])}")
    print(f"  nixai timeout: {len(stats['nixai_timeout'])}")
    print(f"  url parse failed: {len(stats['url_parse_failed'])}")
    if usage_by_package:
        #print("Cost summary per package:")
        #for usage in usage_by_package:
        #    print(
        #        "  {name}: Prompt={prompt} Cached={cached} Completion={completion} Total=${cost:.8f}".format(
        #            name=usage["package"],
        #            prompt=usage["prompt_tokens"],
        #            cached=usage["cached_tokens"],
        #            completion=usage["completion_tokens"],
        #            cost=usage["total_cost"],
        #        )
        #    )
        print("Cost totals across all packages:")
        print(f"  Prompt Tokens: {usage_totals['prompt_tokens']}")
        print(f"  Cached Tokens: {usage_totals['cached_tokens']}")
        print(f"  Completion Tokens: {usage_totals['completion_tokens']}")
        print(f"  Total Cost: ${usage_totals['total_cost']:.8f}")
        total_costs_path = base_output / "total_costs.txt"
        total_costs_content = (
            "Prompt Tokens: {prompt}\n"
            "Cached Tokens: {cached}\n"
            "Completion Tokens: {completion}\n"
            "Total Cost: ${cost:.8f}\n"
        ).format(
            prompt=usage_totals["prompt_tokens"],
            cached=usage_totals["cached_tokens"],
            completion=usage_totals["completion_tokens"],
            cost=usage_totals["total_cost"],
        )
        if args.dry_run:
            print(f"DRY-RUN: write {total_costs_path}")
        else:
            total_costs_path.write_text(total_costs_content, encoding="utf-8")
    if stats["build_succeeded"]:
        print("Build succeeded packages:")
        for item in stats["build_succeeded"]:
            print(f"  {item}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
