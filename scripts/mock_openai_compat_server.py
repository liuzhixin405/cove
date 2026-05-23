from http.server import BaseHTTPRequestHandler, HTTPServer
import json

class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        return

    def do_POST(self):
        if self.path != '/v1/chat/completions':
            self.send_response(404)
            self.end_headers()
            self.wfile.write(b'not found')
            return
        length = int(self.headers.get('Content-Length', '0'))
        body = self.rfile.read(length)
        req = json.loads(body.decode('utf-8'))
        messages = req.get('messages', [])
        user_text = ''
        for msg in reversed(messages):
            if msg.get('role') == 'user':
                user_text = msg.get('content', '')
                break
        reply = f'MOCK_REPLY: {user_text}' if user_text else 'MOCK_REPLY: empty'
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Cache-Control', 'no-cache')
        self.end_headers()
        chunk = {
            'choices': [
                {
                    'delta': {'content': reply}
                }
            ]
        }
        self.wfile.write(f"data: {json.dumps(chunk, ensure_ascii=False)}\n\n".encode('utf-8'))
        self.wfile.write(b'data: [DONE]\n\n')
        self.wfile.flush()

if __name__ == '__main__':
    server = HTTPServer(('127.0.0.1', 18080), Handler)
    print('mock server listening on 127.0.0.1:18080', flush=True)
    server.serve_forever()
