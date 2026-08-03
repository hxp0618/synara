// FILE: SynaraLogo.tsx
// Purpose: Render the shared Synara mark inside the independent Admin host.
// Layer: Admin brand primitive

import { SYNARA_LOGO_PATHS } from "@synara/shared/brand";
import type { SVGProps } from "react";

export function SynaraLogo(props: SVGProps<SVGSVGElement>) {
  const ariaLabel = props["aria-label"];
  return (
    <svg
      viewBox="0 0 470 504"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden={ariaLabel ? undefined : true}
      {...props}
    >
      {SYNARA_LOGO_PATHS.map((path) => (
        <path key={path} d={path} fill="currentColor" />
      ))}
    </svg>
  );
}
