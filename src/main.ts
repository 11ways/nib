#!/usr/bin/env bun
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { createServer } from "./server"

const server = createServer({ directory: process.env.NIB_DIRECTORY || process.cwd(), confirmOutsideRoots: process.env.NIB_CONFIRM_OUTSIDE_ROOTS === "1" })
const shutdown = () => { void server.close().finally(() => process.exit(0)) }
process.stdin.once("close", shutdown)
process.once("SIGTERM", shutdown)
process.once("SIGINT", shutdown)
await server.connect(new StdioServerTransport())
