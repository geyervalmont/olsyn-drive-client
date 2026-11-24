// Quitting from the tray asks whether to also disconnect the drive: the agent
// serves W:, so only "Disconnect and quit" stops it.

import { useAction, useAgentContext, useAgentView } from '../../state/context'
import { Dialog } from '../../ui/Dialog'
import { Button } from '../../ui/controls'
import { plural } from '../../lib/format'

export function QuitDialog({ onClose }: { onClose: () => void }) {
  const view = useAgentView()
  const { client, shell } = useAgentContext()
  const { pending, error, run } = useAction()
  const letter = view.state?.mount.letter ?? 'the drive'
  const left = view.state?.transfers.files_left ?? 0
  return (
    <Dialog
      title="Quit Olsyn Drive?"
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="secondary" onClick={() => shell.quitApp()} data-autofocus>
            Close the app, keep {letter}
          </Button>
          <Button
            variant="danger"
            pending={pending === 'quit'}
            onClick={() =>
              void run('quit', async () => {
                await client.quit(true)
                shell.quitApp()
              })
            }
          >
            Disconnect {letter} and quit
          </Button>
        </>
      }
    >
      <p>Closing the app hides the tray icon; {letter} stays and files keep going up.</p>
      <p>Disconnecting removes {letter} from File Explorer until you start Olsyn Drive again.{left > 0 ? ' ' + plural(left, 'file') + ' still to send will wait on this PC.' : ''}</p>
      {error && <p className="field-error" role="alert">{error.message}</p>}
    </Dialog>
  )
}
