// A file name that never hides its end: the stem shortens with an ellipsis, the
// extension always shows, and the whole name is in the tooltip and read aloud.

export function splitName(name: string): { stem: string; ext: string } {
  const dot = name.lastIndexOf('.')
  if (dot <= 0 || dot === name.length - 1 || name.length - dot > 8) return { stem: name, ext: '' }
  return { stem: name.slice(0, dot), ext: name.slice(dot) }
}

export function FileName({ name, className }: { name: string; className?: string }) {
  const { stem, ext } = splitName(name)
  return (
    <span className={'file-name' + (className ? ' ' + className : '')} title={name}>
      <span className="file-name-stem" aria-hidden="true">
        {stem}
      </span>
      {ext && (
        <span className="file-name-ext" aria-hidden="true">
          {ext}
        </span>
      )}
      <span className="sr-only">{name}</span>
    </span>
  )
}

/** Any one-line text that may be cut: full text in a tooltip. */
export function Truncate({ text, className }: { text: string; className?: string }) {
  return (
    <span className={'truncate' + (className ? ' ' + className : '')} title={text}>
      {text}
    </span>
  )
}
