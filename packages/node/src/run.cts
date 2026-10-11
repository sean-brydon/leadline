import { fstatSync } from "node:fs";

/**
 * What the provider writes before the newline of every console line it has
 * itself sent, when the app runs under `leadline run`. `leadline run` reads
 * the app's output too, to capture what no provider sends; this tells it the
 * line is already in Leadline, with its trace id, so it shows once. It is an
 * APC string, which a terminal shows as nothing, and `leadline run` takes it
 * out before the terminal gets it anyway.
 *
 * internal/runner/mark.go has the same bytes.
 */
export const MARK = "\x1b_leadline:sent\x1b\\";

export type StreamName = "stdout" | "stderr";

/**
 * Which of this process's output streams `leadline run` is reading.
 *
 * `leadline run` names the streams it gave its command in LEADLINE_RUN_STDIO,
 * each as the device and inode `fstat` reports. The variable reaches every
 * process the command starts, but their output does not always reach
 * `leadline run`: `node build.js > out.json` and `$(node -p ...)` write
 * somewhere else, and a mark there would corrupt what they wrote. So a stream
 * is only marked when it is, by `fstat`, one of the streams that were named.
 */
export function runStreams(
  env: NodeJS.ProcessEnv = process.env,
  identify: (fd: number) => string | undefined = streamId,
): Record<StreamName, boolean> {
  const named = new Set(
    (env.LEADLINE_RUN_STDIO ?? "")
      .split(",")
      .map((id) => id.trim())
      .filter(Boolean),
  );
  const read = (fd: number) => {
    if (named.size === 0) return false;
    const id = identify(fd);
    return id !== undefined && named.has(id);
  };
  return { stdout: read(1), stderr: read(2) };
}

/** "dev:ino" for an open file, both as unsigned numbers, the way Go prints them. */
export function streamId(fd: number): string | undefined {
  try {
    const stat = fstatSync(fd, { bigint: true });
    return `${BigInt.asUintN(64, stat.dev)}:${BigInt.asUintN(64, stat.ino)}`;
  } catch {
    return undefined;
  }
}

type Writable = { write: (...args: any[]) => boolean };

/**
 * Marks every line written to `stream` from now until the returned function
 * is called. The console formats its arguments and writes the result in one
 * call, inside the `console.log` call itself, so wrapping `write` for exactly
 * that long marks that text and nothing else.
 */
export function markLines(stream: Writable): () => void {
  const own = Object.hasOwn(stream, "write");
  const write = stream.write;
  const marked = function (this: unknown, chunk: unknown, ...rest: unknown[]): boolean {
    return write.call(
      this,
      typeof chunk === "string" ? chunk.replaceAll("\n", `${MARK}\n`) : chunk,
      ...rest,
    );
  };
  stream.write = marked;
  return () => {
    // Leave it alone if something else has wrapped the stream since.
    if (stream.write !== marked) return;
    if (own) stream.write = write;
    else delete (stream as Partial<Writable>).write;
  };
}
