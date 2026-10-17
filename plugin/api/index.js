import { Inject, Injectable, Module } from '@nestjs/common';
import { HttpAdapterHost } from '@nestjs/core';
import { request } from 'node:http';

// Register through Unraid's API plugin loader, under the existing /graphql
// nginx route. These exact native-only routes use daemon authentication;
// neither a local admin session nor an Unraid internal key is manufactured.
const base = '/graphql/api/mfconnect/v1';
const allowed = new Set(['capability', 'auth', 'targets', 'open', 'push', 'pull', 'close']);
const socketPath = '/var/run/mfconnect/standalone.sock';
export const adapter = 'nestjs';
const error = (code) => ({ protocol: 1, error: { code, retryable: code === 'DAEMON_UNAVAILABLE' } });

export class MFConnectBridge {
  constructor(adapterHost) { this.adapterHost = adapterHost; }
  onModuleInit() {
    const app = this.adapterHost.httpAdapter.getInstance();
    app.route({
      method: ['GET', 'POST'], url: `${base}/:operation`, bodyLimit: 98304,
      logLevel: 'silent',
      errorHandler: (err, _req, reply) => {
        const status = err.statusCode === 413 ? 413 : 400;
        return reply.code(status).send(error(status === 413 ? 'REQUEST_TOO_LARGE' : 'INVALID_REQUEST'));
      },
      handler: async (req, reply) => {
        reply.header('Cache-Control', 'no-store');
        reply.header('X-Content-Type-Options', 'nosniff');
        const op = req.params.operation;
        if (!allowed.has(op)) return reply.code(404).send(error('NOT_FOUND'));
        if (req.headers.origin) return reply.code(403).send(error('ORIGIN_FORBIDDEN'));
        if (Object.keys(req.query || {}).length) return reply.code(400).send(error('INVALID_REQUEST'));
        const method = (op === 'capability' || op === 'targets') ? 'GET' : 'POST';
        if (req.method !== method) return reply.code(400).send(error('INVALID_REQUEST'));
        const payload = method === 'POST' ? Buffer.from(JSON.stringify(req.body ?? null)) : null;
        if (payload && payload.length > 98304) return reply.code(413).send(error('REQUEST_TOO_LARGE'));
        const headers = { 'content-type': 'application/json' };
        for (const name of ['authorization', 'x-api-key', 'cookie', 'x-csrf-token']) {
          if (typeof req.headers[name] === 'string') headers[name] = req.headers[name];
        }
        if (payload) headers['content-length'] = String(payload.length);
        // No outer credential is ever copied to a container request. Only the
        // daemon receives these control headers, over its private Unix socket.
        return await new Promise((resolve) => {
          let settled = false;
          const finish = (status, value) => {
            if (settled) return;
            settled = true;
            resolve(reply.code(status).send(value));
          };
          const upstream = request({ socketPath, path: `${base}/${op}`, method, headers }, (res) => {
            const chunks = []; let size = 0;
            res.on('data', (chunk) => {
              size += chunk.length;
              if (size > 2 * 1024 * 1024) { upstream.destroy(); finish(503, error('DAEMON_UNAVAILABLE')); }
              else chunks.push(chunk);
            });
            res.on('end', () => {
              try { finish(res.statusCode || 503, JSON.parse(Buffer.concat(chunks).toString('utf8'))); }
              catch { finish(503, error('DAEMON_UNAVAILABLE')); }
            });
            res.on('error', () => finish(503, error('DAEMON_UNAVAILABLE')));
          });
          upstream.setTimeout(30000, () => upstream.destroy());
          upstream.on('error', () => finish(503, error('DAEMON_UNAVAILABLE')));
          reply.raw.on('close', () => { if (!settled) upstream.destroy(); });
          if (payload) upstream.write(payload);
          upstream.end();
        });
      },
    });
  }
}
Inject(HttpAdapterHost)(MFConnectBridge, undefined, 0);
Injectable()(MFConnectBridge);
export class ApiModule {}
Module({ providers: [MFConnectBridge] })(ApiModule);
