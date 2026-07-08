/**
 * OrchestrationReactor - Composite orchestration reactor service interface.
 *
 * Coordinates startup of orchestration runtime reactors that translate domain
 * events into downstream side effects.
 *
 * @module OrchestrationReactor
 */
import type { OrchestrationQueuedTurn } from "@t3tools/contracts";
import { ServiceMap } from "effect";
import type { Effect, Scope } from "effect";

/**
 * OrchestrationReactorShape - Service API for orchestration reactor lifecycle.
 */
export interface OrchestrationReactorShape {
  /**
   * Start orchestration-side reactors for provider/runtime/checkpoint flows.
   *
   * The returned effect must be run in a scope so all worker fibers can be
   * finalized on shutdown.
   */
  readonly start: Effect.Effect<void, never, Scope.Scope>;

  /** Restore durable queued turns after projection bootstrap. */
  readonly rehydrateQueuedTurns: (
    turns: ReadonlyArray<OrchestrationQueuedTurn>,
  ) => Effect.Effect<void>;
}

/**
 * OrchestrationReactor - Service tag for orchestration reactor coordination.
 */
export class OrchestrationReactor extends ServiceMap.Service<
  OrchestrationReactor,
  OrchestrationReactorShape
>()("t3/orchestration/Services/OrchestrationReactor") {}
