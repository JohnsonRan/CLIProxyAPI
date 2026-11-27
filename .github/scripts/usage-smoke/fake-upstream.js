// Minimal OpenAI-compatible upstream for the usage smoke test.
// "fail please" in the prompt answers 500; everything else answers a chat completion with usage.
require('http')
  .createServer((req, res) => {
    let body = '';
    req.on('data', (chunk) => (body += chunk));
    req.on('end', () => {
      if (req.url.includes('/models')) {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ data: [{ id: 'gpt-5' }, { id: 'gpt-5-mini' }] }));
        return;
      }
      if (body.includes('fail please')) {
        res.writeHead(500, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ error: { message: 'upstream exploded' } }));
        return;
      }
      const model = JSON.parse(body || '{}').model || 'gpt-5';
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(
        JSON.stringify({
          id: 'x',
          object: 'chat.completion',
          created: 1,
          model,
          choices: [{ index: 0, message: { role: 'assistant', content: 'hi' }, finish_reason: 'stop' }],
          usage: {
            prompt_tokens: 2000,
            completion_tokens: 300,
            total_tokens: 2300,
            prompt_tokens_details: { cached_tokens: 1000 },
            completion_tokens_details: { reasoning_tokens: 80 },
          },
        })
      );
    });
  })
  .listen(Number(process.env.FAKE_PORT || 18999));
