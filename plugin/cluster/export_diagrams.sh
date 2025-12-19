#!/bin/bash

# This script exports the Mermaid diagrams in architecture.md to PNG files
# Requires Node.js, npm, and the mermaid-cli package

echo "Exporting Cluster architecture diagrams..."

# Check if mmdc (mermaid-cli) is installed
if ! command -v mmdc &> /dev/null; then
    echo "mermaid-cli not found. Installing globally..."
    npm install -g @mermaid-js/mermaid-cli
fi

# Create output directory
mkdir -p diagrams

# Extract and convert the main architecture diagram
echo "Extracting main architecture diagram..."
sed -n '/```mermaid/,/```/ p' architecture.md | sed '1d;$d' > diagrams/architecture.mmd
mmdc -i diagrams/architecture.mmd -o diagrams/architecture.png -t neutral -b transparent

# Extract and convert the stream publishing flow
echo "Extracting stream publishing flow diagram..."
sed -n '/### 1\. Stream Publishing Flow/,/```/ p' architecture.md | sed -n '/```mermaid/,/```/ p' | sed '1d;$d' > diagrams/publishing_flow.mmd
mmdc -i diagrams/publishing_flow.mmd -o diagrams/publishing_flow.png -t neutral -b transparent

# Extract and convert the stream consumption flow
echo "Extracting stream consumption flow diagram..."
sed -n '/### 2\. Stream Consumption Flow/,/```/ p' architecture.md | sed -n '/```mermaid/,/```/ p' | sed '1d;$d' > diagrams/consumption_flow.mmd
mmdc -i diagrams/consumption_flow.mmd -o diagrams/consumption_flow.png -t neutral -b transparent

# Extract and convert the node failure handling
echo "Extracting node failure handling diagram..."
sed -n '/### 3\. Node Failure Handling/,/```/ p' architecture.md | sed -n '/```mermaid/,/```/ p' | sed '1d;$d' > diagrams/failure_handling.mmd
mmdc -i diagrams/failure_handling.mmd -o diagrams/failure_handling.png -t neutral -b transparent

# Extract and convert the physical deployment view
echo "Extracting physical deployment view diagram..."
sed -n '/## Physical Deployment View/,/```/ p' architecture.md | sed -n '/```mermaid/,/```/ p' | sed '1d;$d' > diagrams/deployment_view.mmd
mmdc -i diagrams/deployment_view.mmd -o diagrams/deployment_view.png -t neutral -b transparent

# Copy the architecture.png to the root plugin directory for README.md
cp diagrams/architecture.png architecture.png

echo "All diagrams exported successfully!"
echo "Main architecture diagram copied to architecture.png for README.md reference" 