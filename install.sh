#!/bin/bash
set -e

echo "=== Autopeer Agent Installer ==="

# Build binary
echo "Building autopeer-agent..."
go build -o autopeer-agent ./cmd/agent

# Install binary
echo "Installing binary to /usr/local/bin/"
sudo install -m 0700 -o root -g root autopeer-agent /usr/local/bin/autopeer-agent

# Create config directory
sudo install -d -m 0700 -o root -g root /etc/autopeer-agent
sudo install -d -m 0700 -o root -g root /var/lib/autopeer-agent

# Copy example config if no config exists
if [ ! -f /etc/autopeer-agent/config.yaml ]; then
    sudo install -m 0600 -o root -g root config.example.yaml /etc/autopeer-agent/config.yaml
    echo "Config copied to /etc/autopeer-agent/config.yaml - please edit it"
fi

# Install systemd service
echo "Installing systemd service..."
sudo install -m 0644 -o root -g root autopeer-agent.service /etc/systemd/system/autopeer-agent.service
sudo systemctl daemon-reload
sudo systemctl enable autopeer-agent

echo ""
echo "Installation complete!"
echo "1. Edit /etc/autopeer-agent/config.yaml with your settings"
echo "2. Start the service: sudo systemctl start autopeer-agent"
echo "3. Check status: sudo systemctl status autopeer-agent"
