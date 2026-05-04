#!/usr/bin/env python3
"""Discord watcher — monitors @mentions and DMs, sends mail to mayor/."""

import asyncio
import json
import os
import signal
import sys
from pathlib import Path

try:
    import discord
except ImportError:
    print("ERROR: discord.py not installed. Run: pip install discord.py", file=sys.stderr)
    sys.exit(1)


STATE_FILE = Path(os.environ.get("GT_ROOT", os.path.expanduser("~/gt"))) / "daemon" / "discord_watcher_state.json"


def load_state() -> set:
    """Load set of processed message IDs."""
    if STATE_FILE.exists():
        try:
            data = json.loads(STATE_FILE.read_text())
            return set(data.get("processed_ids", []))
        except (json.JSONDecodeError, OSError):
            pass
    return set()


def save_state(processed_ids: set):
    """Persist processed message IDs (keep last 1000 to bound file size)."""
    STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
    trimmed = sorted(processed_ids)[-1000:]
    STATE_FILE.write_text(json.dumps({"processed_ids": trimmed}))


def send_mail(channel_id: str, author: str, content: str, message_id: str):
    """Send mail to mayor/ via gt mail send."""
    subject = f"Discord: {author} in #{channel_id}"
    body = f"Author: {author}\nChannel: {channel_id}\nMessage ID: {message_id}\n\n{content}"
    import subprocess
    try:
        subprocess.run(
            ["gt", "mail", "send", "mayor/", "-s", subject, "-b", body],
            timeout=30,
            capture_output=True,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError, OSError) as e:
        print(f"WARNING: Failed to send mail: {e}", file=sys.stderr)


class WatcherClient(discord.Client):
    def __init__(self, processed_ids: set):
        intents = discord.Intents.default()
        intents.message_content = True
        intents.dm_messages = True
        super().__init__(intents=intents)
        self.processed_ids = processed_ids
        self.bot_user_id = None

    async def on_ready(self):
        self.bot_user_id = self.user.id
        print(f"Discord watcher connected as {self.user}", flush=True)

    async def on_message(self, message: discord.Message):
        if message.author == self.user:
            return

        msg_id = str(message.id)
        if msg_id in self.processed_ids:
            return

        is_mention = self.user in message.mentions
        is_dm = isinstance(message.channel, discord.DMChannel)

        if not is_mention and not is_dm:
            return

        channel_name = getattr(message.channel, "name", "DM")
        send_mail(channel_name, str(message.author), message.content, msg_id)

        self.processed_ids.add(msg_id)
        save_state(self.processed_ids)
        print(f"Processed message {msg_id} from {message.author}", flush=True)


async def main():
    token = os.environ.get("DISCORD_TOKEN")
    if not token:
        print("ERROR: DISCORD_TOKEN not set", file=sys.stderr)
        sys.exit(1)

    processed_ids = load_state()
    client = WatcherClient(processed_ids)

    loop = asyncio.get_event_loop()
    stop_event = asyncio.Event()

    def handle_signal():
        print("Received shutdown signal", flush=True)
        stop_event.set()

    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, handle_signal)

    async with client:
        task = asyncio.create_task(client.start(token))
        await stop_event.wait()
        await client.close()
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass

    save_state(client.processed_ids)
    print("Discord watcher shut down cleanly", flush=True)


if __name__ == "__main__":
    asyncio.run(main())
