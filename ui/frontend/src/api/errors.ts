// The error shape every agent response uses (docs/ipc.md "Errors").

export class AgentError extends Error {
  readonly status: number
  readonly code: string
  readonly detail: Record<string, unknown>

  constructor(status: number, code: string, message: string, detail: Record<string, unknown> = {}) {
    super(message)
    this.name = 'AgentError'
    this.status = status
    this.code = code
    this.detail = detail
  }

  /** "Wait" conditions: the drive or the agent will come back on its own. */
  get isWait(): boolean {
    return this.code === 'offline' || this.code === 'server_restarting' || this.code === 'stopping'
  }
}

/** Raised when the agent's endpoint does not answer at all. */
export class AgentUnreachableError extends Error {
  /** cause is what the page saw (a rejected fetch, say); it is reported to the shell's log. */
  constructor(message = "Olsyn Drive isn't answering on this PC.", cause?: unknown) {
    super(message, cause === undefined ? undefined : { cause })
    this.name = 'AgentUnreachableError'
  }
}

/** A short, log-safe description of why a request failed in the page: the error's name and message, never a body. */
export function describeFailure(err: unknown): string {
  const cause = err instanceof Error && err.cause !== undefined ? err.cause : err
  const text = cause instanceof Error ? cause.name + ': ' + cause.message : String(cause)
  return text.length > 300 ? text.slice(0, 300) : text
}

const fallbackMessages: Record<number, string> = {
  400: 'That request was not understood.',
  404: "That item isn't there any more.",
  409: "That can't be done right now.",
  422: 'That value is not allowed.',
  500: 'Something went wrong inside Olsyn Drive.',
  502: 'The drive answered with an error.',
  503: "The drive can't be reached right now.",
}

export function errorFromResponse(status: number, body: unknown): AgentError {
  if (body && typeof body === 'object' && typeof (body as { error?: unknown }).error === 'string') {
    const { error, message, ...rest } = body as { error: string; message?: unknown }
    const text = typeof message === 'string' && message ? message : (fallbackMessages[status] ?? 'Something went wrong.')
    return new AgentError(status, error, text, rest)
  }
  return new AgentError(status, status >= 500 ? 'internal' : 'bad_request', fallbackMessages[status] ?? 'Something went wrong.')
}
