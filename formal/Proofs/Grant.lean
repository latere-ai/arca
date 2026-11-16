import Machines.Grant

/-!
Claims about the grant table, proved over the table that
`internal/store.GrantTable` renders into `Machines/Grant.lean`.
-/

namespace Proofs.Grant
open Machines.Grant Statemachine

/-- A revoked grant stays revoked: nothing follows `revoked`, so no write
makes it active again. -/
theorem revoked_stays_revoked (s : State) (r : Reaches machine .revoked s) :
    s = .revoked :=
  machine.terminal_stays terminal_revoked r

/-- Only an active grant is revoked: every predecessor of `revoked` is
`active`. -/
theorem revoked_only_after_active :
    ∀ s ∈ machine.states, State.revoked ∈ machine.next s → s = .active := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Grant
