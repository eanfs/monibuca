import yaml
import socket
import argparse
import base64
import time
from typing import List, Dict

class RtspMock:
    def __init__(self, dump_file: str):
        # Load and parse YAML file
        with open(dump_file, 'r') as f:
            data = yaml.safe_load(f)
            
        self.peers = {p['peer']: (p['host'], p['port']) for p in data['peers']}
        self.packets = data['packets']
        
        # Group packets by peer
        self.peer_packets: Dict[int, List] = {}
        for packet in self.packets:
            peer = packet['peer']
            if peer not in self.peer_packets:
                self.peer_packets[peer] = []
            self.peer_packets[peer].append(packet)

    def run_server(self, port: int, peer: int):
        """Run as server listening on specified port"""
        server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        server.bind(('0.0.0.0', port))
        server.listen(1)
        
        print(f"Listening on port {port}, will send peer {peer}'s packets")
        
        while True:
            client, addr = server.accept()
            print(f"Client connected from {addr}")
            try:
                self._send_packets(client, peer)
            except Exception as e:
                print(f"Error: {e}")
            finally:
                client.close()

    def run_client(self, host: str, port: int, peer: int):
        """Run as client connecting to specified address"""
        client = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        print(f"Connecting to {host}:{port}, will send peer {peer}'s packets")
        
        try:
            client.connect((host, port))
            self._send_packets(client, peer)
        except Exception as e:
            print(f"Error: {e}")
        finally:
            client.close()

    def _send_packets(self, sock: socket, peer: int):
        """Send packets for specified peer"""
        if peer not in self.peer_packets:
            raise ValueError(f"No packets found for peer {peer}")
            
        packets = self.peer_packets[peer]
        base_time = None
        
        for packet in packets:
            if base_time is None:
                base_time = packet['timestamp']
                
            # Calculate delay
            delay = packet['timestamp'] - base_time
            if delay > 0:
                time.sleep(delay)
                
            data = packet['data']
            sock.send(data)
            print(f"Sent packet {packet['packet']} (index {packet['index']}) "
                  f"length {len(data)} bytes")

def main():
    parser = argparse.ArgumentParser(description='RTSP Mock Server/Client')
    parser.add_argument('dump_file', help='RTSP dump file in YAML format')
    parser.add_argument('peer', type=int, help='Peer number to mock')
    
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument('-l', '--listen', type=int, metavar='PORT',
                      help='Run as server listening on specified port')
    group.add_argument('-c', '--connect', type=str, metavar='HOST:PORT',
                      help='Run as client connecting to specified address')
    
    args = parser.parse_args()
    
    mock = RtspMock(args.dump_file)
    
    if args.listen is not None:
        mock.run_server(args.listen, args.peer)
    else:
        host, port = args.connect.split(':')
        mock.run_client(host, int(port), args.peer)

if __name__ == '__main__':
    main()