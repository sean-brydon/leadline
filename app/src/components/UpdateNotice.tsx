import { ArrowUpCircleIcon } from "lucide-react";
import { installUpdate, useUpdate } from "@/lib/update";
import { updateOffer } from "@/lib/updateText";
import { Tip } from "@/components/ui";
import { Button } from "@/components/ui/button";

/**
 * UpdateNotice is the window's one word about updates: nothing at all until
 * a newer Leadline is downloaded and checked, then a small button in the
 * bar that restarts into it. The app never restarts by itself: the daemon
 * restarts with it, and when a second of recording is lost is the person's
 * to choose.
 */
export function UpdateNotice() {
  const offer = updateOffer(useUpdate());
  if (!offer) return null;
  return (
    <Tip label={offer.tip}>
      <Button variant="outline" size="xs" onClick={installUpdate} loading={offer.busy} className="shrink-0 font-normal text-muted-foreground hover:text-foreground">
        <ArrowUpCircleIcon />
        {/* At the window's narrowest the icon says it alone. */}
        <span className="max-[1000px]:sr-only">{offer.label}</span>
      </Button>
    </Tip>
  );
}
