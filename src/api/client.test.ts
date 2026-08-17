// Security-audit finding M1: streamChat used to bypass getBackendSrv()
// entirely (a raw fetch() with a manual credentials:'include'), the only
// call in this file that did. Rewritten to use BackendSrv's own chunked()
// streaming primitive instead. Since chunked() returns an RxJS Observable
// (not a Fetch Response body reader), everything here is tested against a
// REAL rxjs Observable built from Uint8Array chunks shaped exactly like
// the real backend's newline-delimited-JSON wire format (see
// resources.go's handleStreamResource / streamChatCompletion) -- not a
// simplified stand-in -- so the buffering/parsing logic is exercised the
// same way it would be against the real network.

import { Observable } from 'rxjs';
import type { FetchResponse } from '@grafana/runtime';

const chunkedMock = jest.fn();
jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    chunked: chunkedMock,
  }),
}));

import { streamChat } from './client';
import type { ChatResponse } from '../context/types';

function bytes(s: string): Uint8Array {
  return new TextEncoder().encode(s);
}

function fakeResponse(data: Uint8Array | undefined): FetchResponse<Uint8Array | undefined> {
  return {
    data,
    status: 200,
    statusText: 'OK',
    ok: true,
    headers: new Headers(),
    redirected: false,
    type: 'basic',
    url: '',
    config: { url: '' },
  };
}

async function collect(iter: AsyncGenerator<ChatResponse>): Promise<ChatResponse[]> {
  const out: ChatResponse[] = [];
  for await (const chunk of iter) {
    out.push(chunk);
  }
  return out;
}

const emptyContext = {};

describe('streamChat', () => {
  beforeEach(() => {
    chunkedMock.mockReset();
  });

  it('parses multiple newline-delimited JSON chunks spread across multiple Observable emissions', async () => {
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        subscriber.next(fakeResponse(bytes(JSON.stringify({ content: 'Hel', done: false } as ChatResponse) + '\n')));
        subscriber.next(fakeResponse(bytes(JSON.stringify({ content: 'lo', done: false } as ChatResponse) + '\n')));
        subscriber.next(fakeResponse(bytes(JSON.stringify({ content: '', done: true } as ChatResponse) + '\n')));
        subscriber.complete();
      })
    );

    const chunks = await collect(streamChat('chat', 'hi', emptyContext));
    expect(chunks).toEqual([
      { content: 'Hel', done: false },
      { content: 'lo', done: false },
      { content: '', done: true },
    ]);
  });

  it('reassembles one JSON message split across two separate byte chunks (a chunk boundary landing mid-line)', async () => {
    const wholeLine = JSON.stringify({ content: 'split across a boundary', done: false } as ChatResponse) + '\n';
    const splitPoint = Math.floor(wholeLine.length / 2);

    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        subscriber.next(fakeResponse(bytes(wholeLine.slice(0, splitPoint))));
        subscriber.next(fakeResponse(bytes(wholeLine.slice(splitPoint))));
        subscriber.next(fakeResponse(bytes(JSON.stringify({ content: '', done: true } as ChatResponse) + '\n')));
        subscriber.complete();
      })
    );

    const chunks = await collect(streamChat('chat', 'hi', emptyContext));
    expect(chunks[0]).toEqual({ content: 'split across a boundary', done: false });
  });

  it('stops iterating as soon as done:true arrives, even if the Observable has more to emit', async () => {
    let emittedAfterDone = false;
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        subscriber.next(fakeResponse(bytes(JSON.stringify({ content: 'a', done: true } as ChatResponse) + '\n')));
        // A well-behaved backend wouldn't send more after done:true, but
        // prove the generator doesn't wait around for it regardless.
        setTimeout(() => {
          emittedAfterDone = true;
          subscriber.next(fakeResponse(bytes(JSON.stringify({ content: 'b', done: false } as ChatResponse) + '\n')));
          subscriber.complete();
        }, 50);
      })
    );

    const chunks = await collect(streamChat('chat', 'hi', emptyContext));
    expect(chunks).toEqual([{ content: 'a', done: true }]);
    expect(emittedAfterDone).toBe(false);
  });

  it('flushes a trailing JSON line with no terminating newline once the Observable completes', async () => {
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        // No trailing \n -- the backend's very last write before closing
        // the connection isn't guaranteed to end in one.
        subscriber.next(fakeResponse(bytes(JSON.stringify({ content: 'no trailing newline', done: true } as ChatResponse))));
        subscriber.complete();
      })
    );

    const chunks = await collect(streamChat('chat', 'hi', emptyContext));
    expect(chunks).toEqual([{ content: 'no trailing newline', done: true }]);
  });

  it('throws the backend error message when the Observable errors with a non-2xx-shaped FetchError', async () => {
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        subscriber.error({ status: 413, data: { error: 'request body too large' }, message: 'Request Entity Too Large' });
      })
    );

    await expect(collect(streamChat('chat', 'hi', emptyContext))).rejects.toThrow('request body too large');
  });

  it('throws the backend error message when chunked() returns the error body as bytes', async () => {
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        subscriber.error({
          status: 400,
          data: bytes(JSON.stringify({ error: 'The current model does not support image attachments.' })),
          message: 'Bad Request',
        });
      })
    );

    await expect(collect(streamChat('chat', 'hi', emptyContext))).rejects.toThrow('model does not support image attachments');
  });

  it('throws the backend error message when chunked() returns the error body as an ArrayBuffer', async () => {
    const encoded = bytes(JSON.stringify({ error: 'The current model/provider could not process this image attachment.' }));
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        subscriber.error({
          status: 502,
          data: encoded.buffer,
          message: 'Bad Gateway',
        });
      })
    );

    await expect(collect(streamChat('chat', 'hi', emptyContext))).rejects.toThrow('could not process this image attachment');
  });

  it('normalizes a cancelled request into a real AbortError, matching what ChatInterface.tsx checks for', async () => {
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        // getBackendSrv().chunked() marks an aborted request this way --
        // not a DOMException named "AbortError" like raw fetch() gave us.
        subscriber.error({ cancelled: true, status: 0, data: null });
      })
    );

    let caught: any;
    try {
      await collect(streamChat('chat', 'hi', emptyContext));
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeDefined();
    expect(caught.name).toBe('AbortError');
  });

  it('passes the AbortSignal through to chunked() so an in-flight request can actually be cancelled', async () => {
    chunkedMock.mockReturnValue(
      new Observable<FetchResponse<Uint8Array | undefined>>((subscriber) => {
        subscriber.next(fakeResponse(bytes(JSON.stringify({ content: '', done: true } as ChatResponse) + '\n')));
        subscriber.complete();
      })
    );
    const controller = new AbortController();

    await collect(streamChat('chat', 'hi', emptyContext, undefined, controller.signal));

    expect(chunkedMock).toHaveBeenCalledWith(
      expect.objectContaining({ abortSignal: controller.signal, method: 'POST' })
    );
  });
});
