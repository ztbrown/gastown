#!/usr/bin/env python3
"""
Discord Watcher - Monitors Discord for @mentions and DMs
Sends mail to mayor/ when activity is detected
"""

import os
import sys
import json
import signal
import asyncio
import logging
from pathlib import Path
from typing import Set
import discord
from discord.ext import commands

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    handlers=[
        logging.FileHandler('daemon/discord_watcher.log'),
        logging.StreamHandler(sys.stdout)
    ]
)
logger = logging.getLogger(__name__)

# State file to track processed messages
STATE_FILE = Path('daemon/discord_watcher_state.json')

class DiscordWatcher:
    def __init__(self, token: str):
        self.token = token
        self.processed_messages: Set[int] = set()
        self.shutdown_event = asyncio.Event()

        # Set up intents - we need message content and DMs
        intents = discord.Intents.default()
        intents.message_content = True
        intents.dm_messages = True
        intents.guild_messages = True
        intents.members = True

        self.bot = commands.Bot(command_prefix='!', intents=intents)
        self._setup_handlers()
        self._load_state()

    def _load_state(self):
        """Load processed message IDs from state file"""
        if STATE_FILE.exists():
            try:
                with open(STATE_FILE, 'r') as f:
                    data = json.load(f)
                    self.processed_messages = set(data.get('processed_messages', []))
                logger.info(f"Loaded {len(self.processed_messages)} processed message IDs from state")
            except Exception as e:
                logger.error(f"Failed to load state: {e}")
                self.processed_messages = set()
        else:
            logger.info("No state file found, starting fresh")

    def _save_state(self):
        """Save processed message IDs to state file"""
        try:
            STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
            with open(STATE_FILE, 'w') as f:
                # Only keep last 1000 message IDs to prevent unbounded growth
                recent_messages = list(self.processed_messages)[-1000:]
                json.dump({'processed_messages': recent_messages}, f)
            logger.debug(f"Saved {len(recent_messages)} processed message IDs to state")
        except Exception as e:
            logger.error(f"Failed to save state: {e}")

    def _setup_handlers(self):
        """Set up Discord event handlers"""

        @self.bot.event
        async def on_ready():
            logger.info(f"Discord watcher connected as {self.bot.user}")
            logger.info(f"Bot ID: {self.bot.user.id}")

        @self.bot.event
        async def on_message(message):
            # Ignore messages from the bot itself
            if message.author == self.bot.user:
                return

            # Check if already processed
            if message.id in self.processed_messages:
                logger.debug(f"Skipping already processed message {message.id}")
                return

            should_notify = False
            notification_type = None

            # Check for DMs
            if isinstance(message.channel, discord.DMChannel):
                should_notify = True
                notification_type = "DM"
                logger.info(f"DM from {message.author}: {message.content[:50]}")

            # Check for @mentions
            elif self.bot.user in message.mentions:
                should_notify = True
                notification_type = "MENTION"
                logger.info(f"Mentioned in #{message.channel} by {message.author}: {message.content[:50]}")

            if should_notify:
                await self._send_notification(message, notification_type)
                self.processed_messages.add(message.id)
                self._save_state()

    async def _send_notification(self, message: discord.Message, msg_type: str):
        """Send mail notification to mayor/"""
        try:
            # Prepare message details
            channel_id = str(message.channel.id)
            channel_name = getattr(message.channel, 'name', 'DM')
            author = str(message.author)
            author_id = str(message.author.id)
            content = message.content[:500]  # Truncate long messages
            message_id = str(message.id)

            # Build subject and body
            subject = f"Discord {msg_type}: {author}"
            body = f"""Discord {msg_type} received

From: {author} (ID: {author_id})
Channel: {channel_name} (ID: {channel_id})
Message ID: {message_id}

Content:
{content}

---
Reply using Discord MCP tools:
- Send message: mcp__discord-mcp__send_message or send_private_message
- React: mcp__discord-mcp__add_reaction
"""

            # Send mail using gt mail send
            cmd = [
                'gt', 'mail', 'send', 'mayor/',
                '-s', subject,
                '-m', body,
                '--notify'
            ]

            proc = await asyncio.create_subprocess_exec(
                *cmd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE
            )

            stdout, stderr = await proc.communicate()

            if proc.returncode == 0:
                logger.info(f"Mail sent to mayor/ for {msg_type} from {author}")
            else:
                logger.error(f"Failed to send mail: {stderr.decode()}")

        except Exception as e:
            logger.error(f"Error sending notification: {e}")

    async def run(self):
        """Start the Discord watcher"""
        logger.info("Starting Discord watcher...")

        # Set up graceful shutdown
        loop = asyncio.get_event_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, lambda: asyncio.create_task(self.shutdown()))

        try:
            await self.bot.start(self.token)
        except Exception as e:
            logger.error(f"Error running Discord bot: {e}")
            raise

    async def shutdown(self):
        """Gracefully shut down the watcher"""
        logger.info("Shutting down Discord watcher...")
        self._save_state()
        await self.bot.close()
        self.shutdown_event.set()

def main():
    # Get Discord token from environment or .mcp.json
    token = os.getenv('DISCORD_TOKEN')

    if not token:
        # Try loading from .mcp.json
        try:
            mcp_config_path = Path('.mcp.json')
            if mcp_config_path.exists():
                with open(mcp_config_path, 'r') as f:
                    mcp_config = json.load(f)
                    # Look for discord-mcp server config
                    for server_name, server_config in mcp_config.get('mcpServers', {}).items():
                        if 'discord' in server_name.lower():
                            token = server_config.get('env', {}).get('DISCORD_TOKEN')
                            if token:
                                logger.info(f"Found Discord token in .mcp.json under {server_name}")
                                break
        except Exception as e:
            logger.warning(f"Could not load token from .mcp.json: {e}")

    if not token:
        logger.error("DISCORD_TOKEN not found in environment or .mcp.json")
        sys.exit(1)

    logger.info("Discord token found, initializing watcher...")
    watcher = DiscordWatcher(token)

    try:
        asyncio.run(watcher.run())
    except KeyboardInterrupt:
        logger.info("Received keyboard interrupt")
    except Exception as e:
        logger.error(f"Fatal error: {e}")
        sys.exit(1)

if __name__ == '__main__':
    main()
