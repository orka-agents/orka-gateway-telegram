# Telegram Group Chats Setup & Usage Guide

This guide walks you through setting up and using `orka-gateway-telegram` in Telegram group chats and supergroups.

---

## 1. Prerequisites & Scope

Before configuring group chat support, ensure you have:
- A successfully deployed and running `orka-gateway-telegram` adapter connected to your Orka installation (following the private-chat setup in the main [README](../README.md)).
- Bot credentials and access to configure environment variables.
- A dedicated Telegram test group or supergroup where members are aware the bot is present.

### What Version 1 Supports
- Ordinary groups and supergroups **without** forum topics enabled.
- Explicit command invocation via `/ask@your_bot_username <question>` or direct text replies to the bot's messages.
- Session isolation: each person's conversation history remains separate, and group interactions remain distinct from private chats with the bot.

---

## 2. Telegram Privacy Mode & Permissions

Telegram bots operate under **Privacy Mode** by default. 
- **Keep Privacy Mode Enabled:** Under privacy mode, the bot only receives messages that start with a command (e.g., `/ask`) or are direct replies to the bot's own messages.
- **No Admin Rights Needed:** You do **not** need to grant the bot administrator privileges, nor do you need to disable Telegram's privacy mode for the documented flows to work.

---

## 3. Configuration

To enable group chats, configure the following environment variables in your adapter deployment:

1. **Enable Group Chats:**
   ```env
   TELEGRAM_ALLOW_GROUP_CHATS=true
   ```
2. **Orka Binding Configuration:**
   Configure your Orka gateway binding with:
   - Exact Telegram Bot ID / Username
   - Group Chat ID (Negative integer, e.g., `-1001234567890`)
   - Allowed Sender IDs (List of authorized user Telegram IDs permitted to trigger agent tasks)

---

## 4. Discovering Your Group ID

Group IDs in Telegram are negative integers (usually starting with `-100` for supergroups). Note that the private-chat `/whoami` command will **not** reveal a group ID.

**Recommended Read-Only Discovery Method:**
1. Temporarily add the bot to the target group with privacy mode enabled.
2. Send a test message or command in the group.
3. Inspect your gateway adapter logs or event output to view the incoming update payload, which contains `message.chat.id` (a negative number).

---

## 5. Usage & Interaction

### Asking a Question
To ask the bot a question in the group, address it explicitly using its username: