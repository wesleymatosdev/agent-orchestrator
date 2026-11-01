import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { HoverCard, HoverCardContent, HoverCardTrigger } from "./hover-card";

/** Controlled harness: the dismiss path must reach the app's onOpenChange. */
function Harness({ onOpenChange }: { onOpenChange: (open: boolean) => void }) {
	const [open, setOpen] = useState(false);
	return (
		<HoverCard open={open} onOpenChange={(next) => { setOpen(next); onOpenChange(next); }}>
			<HoverCardTrigger>trigger</HoverCardTrigger>
			<HoverCardContent>
				<div data-testid="card-scroll-area" style={{ overflowY: "auto", height: 20 }}>
					<div style={{ height: 200 }}>card body</div>
				</div>
			</HoverCardContent>
		</HoverCard>
	);
}

function fireScroll(target: Element | Window) {
	target.dispatchEvent(new Event("scroll", { bubbles: target instanceof Element }));
}

describe("HoverCard scroll dismissal", () => {
	it("dismisses when the page scrolls while the card is open", async () => {
		const onOpenChange = vi.fn();
		const user = userEvent.setup();
		const view = render(<Harness onOpenChange={onOpenChange} />);
		await user.hover(view.getByText("trigger"));
		// Radix opens after its open delay; hover-card content lives in a portal.
		await vi.waitFor(() => expect(view.getByTestId("card-scroll-area")).toBeInTheDocument());
		expect(onOpenChange).toHaveBeenCalledWith(true);

		fireScroll(window);
		expect(onOpenChange).toHaveBeenCalledWith(false);
	});

	it("does not dismiss when the scroll originates inside the card", async () => {
		const onOpenChange = vi.fn();
		const user = userEvent.setup();
		const view = render(<Harness onOpenChange={onOpenChange} />);
		await user.hover(view.getByText("trigger"));
		await vi.waitFor(() => expect(view.getByTestId("card-scroll-area")).toBeInTheDocument());

		fireScroll(view.getByTestId("card-scroll-area"));
		expect(onOpenChange).not.toHaveBeenCalledWith(false);
	});

	it("ignores scroll while closed", () => {
		const onOpenChange = vi.fn();
		const view = render(<Harness onOpenChange={onOpenChange} />);
		fireScroll(view.getByText("trigger"));
		expect(onOpenChange).not.toHaveBeenCalled();
	});
});
