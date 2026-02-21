#!/bin/bash
# Start the Comptroller agent in a tmux session
SESSION="hq-comptroller"

if tmux has-session -t "$SESSION" 2>/dev/null; then
    echo "Comptroller already running in $SESSION"
    exit 0
fi

tmux new-session -d -s "$SESSION" -c "$HOME/gt/comptroller"
tmux send-keys -t "$SESSION" 'claude --dangerously-skip-permissions' Enter
sleep 5
tmux send-keys -t "$SESSION" 'Run your patrol cycle. Read CLAUDE.md for your role, then state.json and settings.json. Execute one full patrol cycle using ccusage. Send budget mail via gt mail send. Update state.json when done. Then exit.' Enter

echo "Comptroller started in tmux session: $SESSION"
