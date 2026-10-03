#!/usr/bin/env python3
"""Exhaustively verify the candy puzzle without calling a model or network.

Run: python tools/candy_strategy_verify.py
Shapes may be selected by touch, but flavors cannot be selected in the bag.
A fixed strategy selects r round candies and s star candies.  Every possible
flavor composition at those shape totals is checked against the success rule.
"""

from __future__ import annotations

import argparse
import json
from itertools import product
from pathlib import Path


CAPACITIES = {
    "round": {"apple": 7, "peach": 9, "watermelon": 8},
    "star": {"apple": 7, "peach": 6, "watermelon": 4},
}
FIELDS = tuple((shape, flavor) for shape, flavors in CAPACITIES.items() for flavor in flavors)


def pack_composition(values: tuple[int, ...]) -> dict[str, dict[str, int]]:
    result = {shape: {} for shape in CAPACITIES}
    for (shape, flavor), count in zip(FIELDS, values):
        result[shape][flavor] = count
    return result


def successful(values: tuple[int, ...]) -> bool:
    round_apple, round_peach, _, star_apple, star_peach, _ = values
    return bool((round_apple and star_peach) or (round_peach and star_apple))


def verify() -> dict:
    witnesses: dict[tuple[int, int], tuple[int, ...]] = {}
    examined = 0
    failed_compositions = 0
    blind_maximum = -1
    blind_witness = None
    ranges = [range(CAPACITIES[shape][flavor] + 1) for shape, flavor in FIELDS]
    for values in product(*ranges):
        examined += 1
        if successful(values):
            continue
        failed_compositions += 1
        shape_totals = (sum(values[:3]), sum(values[3:]))
        witnesses.setdefault(shape_totals, values)
        total = sum(shape_totals)
        if total > blind_maximum:
            blind_maximum = total
            blind_witness = values

    strategies = []
    round_total = sum(CAPACITIES["round"].values())
    star_total = sum(CAPACITIES["star"].values())
    for round_count in range(round_total + 1):
        for star_count in range(star_total + 1):
            witness = witnesses.get((round_count, star_count))
            strategies.append({
                "round": round_count,
                "star": star_count,
                "total": round_count + star_count,
                "guarantees_success": witness is None,
                "failure_witness": pack_composition(witness) if witness is not None else None,
            })

    winning = [strategy for strategy in strategies if strategy["guarantees_success"]]
    minimum = min(strategy["total"] for strategy in winning)
    optimum = [strategy for strategy in winning if strategy["total"] == minimum]
    losing_at_twenty = [strategy for strategy in strategies if strategy["total"] == 20]
    assert examined == 201_600
    assert minimum == 21
    assert [(item["round"], item["star"]) for item in optimum] == [(9, 12)]
    assert all(not strategy["guarantees_success"] for strategy in losing_at_twenty)
    assert all(not strategy["guarantees_success"] for strategy in strategies if strategy["total"] <= 20)
    assert blind_maximum + 1 == 29

    return {
        "assumptions": {
            "shape_selectable_by_touch": True,
            "flavor_selectable_before_drawing": False,
            "strategy": "A predetermined count for each shape; no replacement.",
            "success": "round apple + star peach OR round peach + star apple",
        },
        "capacities": CAPACITIES,
        "compositions_examined": examined,
        "failure_compositions": failed_compositions,
        "strategies_examined": len(strategies),
        "minimum_guaranteed_draws": minimum,
        "optimal_strategies": optimum,
        "proof": {
            "upper_bound": [
                "Twelve stars contain apple: without apple there are at most 6 + 4 = 10 stars.",
                "Twelve stars contain peach: without peach there are at most 7 + 4 = 11 stars.",
                "Nine rounds contain apple or peach because only 8 rounds are watermelon.",
                "Either non-watermelon round pairs with the opposite flavor among the stars.",
            ],
            "failure_rectangles": [
                {"round_max": 8, "star_max": 17, "missing": ["round.apple", "round.peach"]},
                {"round_max": 17, "star_max": 10, "missing": ["round.apple", "star.apple"]},
                {"round_max": 15, "star_max": 11, "missing": ["round.peach", "star.peach"]},
                {"round_max": 24, "star_max": 4, "missing": ["star.apple", "star.peach"]},
            ],
            "lower_bound": "Every feasible fixed shape split with total <= 20 has an enumerated failure witness.",
        },
        "twenty_draw_counterexamples": losing_at_twenty,
        "if_shape_selection_is_ignored": {
            "minimum_guaranteed_draws": blind_maximum + 1,
            "largest_failing_draw": blind_maximum,
            "failure_witness": pack_composition(blind_witness),
        },
        "all_fixed_shape_strategies": strategies,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=Path(__file__).resolve().parents[1] / ".local" / "mother-benchmark" / "candy-math.json",
    )
    args = parser.parse_args()
    result = verify()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({
        "minimum_guaranteed_draws": result["minimum_guaranteed_draws"],
        "optimal_shape_counts": [
            {"round": strategy["round"], "star": strategy["star"]}
            for strategy in result["optimal_strategies"]
        ],
        "without_shape_selection": result["if_shape_selection_is_ignored"]["minimum_guaranteed_draws"],
        "compositions_examined": result["compositions_examined"],
        "strategies_examined": result["strategies_examined"],
        "output": str(args.output.resolve()),
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
