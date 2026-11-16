import Machines.File

/-!
Claims about the file row table, proved over the table that
`internal/store.FileTable` renders into `Machines/File.lean`.
-/

namespace Proofs.File
open Machines.File Statemachine

/-- A purged row never comes back: nothing follows `purged`, so no write
makes it live or trashed again. -/
theorem purged_stays_purged (s : State) (r : Reaches machine .purged s) :
    s = .purged :=
  machine.terminal_stays terminal_purged r

/-- A row is restored only from the trash: every predecessor of `live` is
`trashed`. -/
theorem live_only_after_trash :
    ∀ s ∈ machine.states, State.live ∈ machine.next s → s = .trashed := by
  decide

/-- A row is trashed only from live: every predecessor of `trashed` is
`live`. -/
theorem trashed_only_from_live :
    ∀ s ∈ machine.states, State.trashed ∈ machine.next s → s = .live := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.File
