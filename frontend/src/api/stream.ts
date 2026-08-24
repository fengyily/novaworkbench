import { API_BASE, clearToken, getToken } from './client';

// Fetch-based replacement for EventSource that can attach the bearer token
// (EventSource cannot set request headers, so under auth every job stream it
// opened returned 401). Parses `data: <json>\n\n` SSE frames and invokes
// onMessage with the decoded object per event; onError fires once on a non-2xx
// response, a network failure, or the stream ending without the terminal frame.
// A 401 clears the session and bounces to /login like the request<T> wrapper.

export interface EventStream {
  close: () => void;
}

export function createEventStream(
  path: string,
  onMessage: (data: any) => void,
  onError: () => void,
): EventStream {
  const controller = new AbortController();
  let closed = false;
  let finished = false;

  const fail = () => {
    if (finished) return;
    finished = true;
    onError();
  };

  // Accept a relative path (prepends API_BASE) or an absolute URL as-is.
  const url = path.startsWith('http') ? path : `${API_BASE}${path}`;

  // Cache-Control: no-cache prevents intermediate proxies from caching the
  // SSE response; X-Accel-Buffering: no on the response side (see
  // backend/internal/handler/response.go writeSSEHeaders) handles nginx
  // proxy buffering.
  const headers: Record<string, string> = {
    Accept: 'text/event-stream',
    'Cache-Control': 'no-cache',
  };
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;

  void (async () => {
    try {
      const res = await fetch(url, { headers, signal: controller.signal });
      if (res.status === 401) {
        clearToken();
        if (location.pathname !== '/login') location.replace('/login');
        fail();
        return;
      }
      if (!res.ok || !res.body) {
        fail();
        return;
      }

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let sep: number;
        while ((sep = buf.indexOf('\n\n')) !== -1) {
          const frame = buf.slice(0, sep);
          buf = buf.slice(sep + 2);
          for (const line of frame.split('\n')) {
            const data = line.startsWith('data:') ? line.slice(5).trim() : line.trim();
            if (!data) continue;
            try {
              onMessage(JSON.parse(data));
            } catch {
              // skip malformed frame
            }
          }
        }
      }
      // Stream ended without the terminal (job_done) frame.
      if (!closed) fail();
    } catch {
      // AbortError from close() is expected; anything else is a dropped link.
      if (!closed) fail();
    }
  })();

  return {
    close: () => {
      closed = true;
      controller.abort();
    },
  };
}
