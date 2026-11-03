#!/usr/bin/env python3
"""check_admin_endpoints.py — 后台接口与控制台页面调用点的落差检查

用途：防止"后端加了服务、前端没有入口"变成死功能。规则来源：项目约定
「后端有的服务不能成为死服务，新加的功能在前端都要有对应的配置点」。

做法（纯静态、不连库不联网）：
  1. 从 manager/backend/router/router.go 抽出路由：
       admin.*  （管理端，需要前端页面/按钮）
       auth.*   （登录用户端，可能被前端任意页调用）
       internal.*（主服务↔控制台内部接口，**不应**有前端调用点）
  2. 从 manager/frontend/src/** 抽 api.<method>('<path>') 调用点
  3. 归一化路径（去 query、`${x}`→`:p`、去掉参数名差异）后比对

判定：
  - admin 路由无调用点        → WARN（可能是漏做 UI，也可能是被其它页面以动态路径调用）
  - internal 路由有调用点      → WARN（前端不应直连内部接口）
  - 前端调用但后端无对应路由   → WARN（拼错路径或路由还没注册）

用法：
  python manager/tools/check_admin_endpoints.py            # 报告，退出码 0
  python manager/tools/check_admin_endpoints.py --strict   # 有 WARN 即退出码 1（提交前可用）
  python manager/tools/check_admin_endpoints.py --admin-only  # 只看管理端缺口
"""
from __future__ import annotations

import argparse
import glob
import os
import re
import sys

ROUTER = "manager/backend/router/router.go"
FRONTEND_SRC = "manager/frontend/src"

ROUTE_RE = re.compile(r'^\s*(admin|auth|user|internal|openV1)\.(GET|POST|PUT|DELETE|PATCH)\("([^"]+)"', re.M)

# 路由分组 → 实际挂载前缀（与 router.go 里的 Group() 保持一致）
GROUP_PREFIX = {
    "admin": "/api/admin",
    "user": "/api/user",
    "auth": "/api",
    "internal": "/api",
    "openV1": "/api/open/v1",
}
CALL_RE = re.compile(r"api\.(get|post|put|delete|patch)\(\s*[`'\"]([^`'\"]+)[`'\"]", re.I)


def normalize(path: str) -> str:
    """把 path 归一成 method 无关的比较形式：去 query、模板变量与 :param 统一成 {}。"""
    path = path.split("?")[0].strip()
    path = re.sub(r"\$\{[^}]*\}", "{}", path)
    path = re.sub(r":[A-Za-z_][A-Za-z0-9_]*", "{}", path)
    path = re.sub(r"\{\{[^}]*\}\}", "{}", path)
    path = re.sub(r"/+", "/", path)
    return path.rstrip("/") or "/"


def load_routes(root: str) -> dict[str, list[tuple[str, str, str]]]:
    """返回 {group: [(method, normalized_path, raw_path)]}"""
    groups: dict[str, list[tuple[str, str, str]]] = {}
    with open(os.path.join(root, ROUTER), encoding="utf-8", errors="ignore") as fh:
        text = fh.read()
    for group, method, path in ROUTE_RE.findall(text):
        prefix = GROUP_PREFIX.get(group, "/api")
        full = path if path.startswith("/api") else prefix + path
        groups.setdefault(group, []).append((method.upper(), normalize(full), path))
    return groups


def load_calls(root: str) -> dict[str, set[str]]:
    """返回 {method: {normalized_path}}，只保留 /api 前缀（前端 baseURL=/api 已剥掉）"""
    calls: dict[str, set[str]] = {}
    files = glob.glob(os.path.join(root, FRONTEND_SRC, "**", "*.js"), recursive=True)
    files += glob.glob(os.path.join(root, FRONTEND_SRC, "**", "*.vue"), recursive=True)
    for fp in files:
        with open(fp, encoding="utf-8", errors="ignore") as fh:
            text = fh.read()
        for method, path in CALL_RE.findall(text):
            full = path if path.startswith("/api") else "/api" + path
            calls.setdefault(method.upper(), set()).add(normalize(full))
    return calls


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=".", help="仓库根目录（默认当前目录）")
    ap.add_argument("--strict", action="store_true", help="有 WARN 时退出码 1")
    ap.add_argument("--admin-only", action="store_true", help="只检查管理端缺口")
    args = ap.parse_args()

    root = os.path.abspath(args.root)
    routes = load_routes(root)
    calls = load_calls(root)

    def has_call(method: str, norm: str) -> bool:
        # 前端用更长的具体路径调同一路由（如带 id）也算命中：按前缀宽松匹配
        for called in calls.get(method, set()):
            if called == norm:
                return True
            if called.startswith(norm + "/"):
                return True
        return False

    all_called: set[str] = set()
    for paths in calls.values():
        all_called |= paths

    warnings: list[str] = []

    for group in ("admin", "user", "auth", "openV1", "internal"):
        if args.admin_only and group != "admin":
            continue
        for method, norm, raw in sorted(routes.get(group, []), key=lambda r: r[2]):
            called = has_call(method, norm)
            if group == "internal" and called:
                warnings.append(f"[内部接口被前端直连] {method} {raw}")
            elif group != "internal" and not called:
                warnings.append(f"[无前端调用点] {group}: {method} {raw}")

    # 前端调了但后端没这条：多半是拼错或路由没注册
    known = {norm for group in ("admin", "user", "auth", "openV1", "internal") for _, norm, _ in routes.get(group, [])}
    for method, norm in sorted((m, p) for m, ps in calls.items() for p in ps):
        if norm in known:
            continue
        if any(norm.startswith(k + "/") or k.startswith(norm + "/") for k in known):
            continue
        # 前端调用 GraphQL/外部代理等非本表路由时会有噪声，统一降级为提示
        warnings.append(f"[前端调用无对应路由] {method} {norm}")

    admin_total = len(routes.get("admin", []))
    admin_called = sum(1 for m, n, _ in routes.get("admin", []) if has_call(m, n))
    print(f"路由统计：admin {admin_total} 条（前端命中 {admin_called}）"
          f"，user {len(routes.get('user', []))}，auth {len(routes.get('auth', []))}"
          f"，internal {len(routes.get('internal', []))}，openV1 {len(routes.get('openV1', []))}")

    if not warnings:
        print("OK  没有发现管理端缺口")
        return 0

    print(f"\n发现 {len(warnings)} 条提示：")
    for w in warnings:
        print("  WARN  " + w)
    return 1 if args.strict else 0


if __name__ == "__main__":
    sys.exit(main())
