"""用真实 MCP Python SDK 连接本服务，复刻 LangBot 的 Streamable HTTP 客户端代码路径。

LangBot 侧对应实现见其 provider/tools/loaders/mcp.py 的 _streamable_http_session：
注入自建 httpx.AsyncClient（带 headers/timeout/follow_redirects），
进入 streamable_http_client，再在同一上下文内 ClientSession.initialize()。
"""

import os

import anyio
import httpx
from mcp import ClientSession

# 可用环境变量覆盖，便于打线上地址：
#   REPOMCP_PROBE_URL=https://repomcp.zerx.dev/mcp REPOMCP_PROBE_TOKEN=xxx
URL = os.getenv("REPOMCP_PROBE_URL", "http://127.0.0.1:8790/mcp")
HEADERS = {"Authorization": "Bearer " + os.getenv("REPOMCP_PROBE_TOKEN", "test-token-abc123")}


async def open_transport(stack):
    """兼容新旧两种 SDK 入口。"""
    try:
        from mcp.client.streamable_http import streamable_http_client

        client = await stack.enter_async_context(
            httpx.AsyncClient(headers=HEADERS, timeout=15, follow_redirects=True)
        )
        return await stack.enter_async_context(
            streamable_http_client(URL, http_client=client)
        ), "streamable_http_client(http_client=...)"
    except ImportError:
        from mcp.client.streamable_http import streamablehttp_client

        return await stack.enter_async_context(
            streamablehttp_client(URL, headers=HEADERS, timeout=15)
        ), "streamablehttp_client(headers=...)"


async def main():
    from contextlib import AsyncExitStack

    async with AsyncExitStack() as stack:
        transport, entry = await open_transport(stack)
        read, write = transport[0], transport[1]
        print(f"transport   : {entry}")

        session = await stack.enter_async_context(ClientSession(read, write))
        init = await session.initialize()
        d = init.model_dump(by_alias=True)
        print(f"protocol    : {d['protocolVersion']}")
        print(f"serverInfo  : {d['serverInfo']['name']} {d['serverInfo']['version']}")
        print(f"capabilities: {d['capabilities']}")
        print(f"instructions: {len(d.get('instructions') or '')} 字符")

        tools = (await session.list_tools()).tools
        print(f"\ntools ({len(tools)}):")
        for t in tools:
            td = t.model_dump(by_alias=True)
            print(f"  - {td['name']:<14} required={td['inputSchema'].get('required', [])}")

        def show(r, n=6):
            rd = r.model_dump(by_alias=True)
            print(f"isError={rd['isError']}")
            print("\n".join(rd["content"][0]["text"].splitlines()[:n]))

        print("\n--- tools/call: find_symbol ---")
        show(await session.call_tool("find_symbol", {"name": "aria2StatusStr", "repo": "fluxdown"}))

        print("\n--- tools/call: search_code（跨仓）---")
        show(await session.call_tool("search_code", {"query": "jsDelivr 测速", "k": 2}), 5)

        print("\n--- tools/call: 错误路径应为 isError 而非协议异常 ---")
        show(await session.call_tool("read_file", {"repo": "fluxdown", "path": "nope.rs"}), 2)

        print("\n--- ping ---")
        await session.send_ping()
        print("ping ok")

    print("\nALL OK")


anyio.run(main)
