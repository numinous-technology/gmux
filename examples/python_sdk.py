"""Drive gmux from Python. Start `gmux serve --fake 1xH100:80G` first."""
import gmux

c = gmux.Client()
print("cards:", [(d["name"], d["free_seats"]) for d in c.cards()])

jobs = [c.run(["sleep", "20"], share=0.25, name=f"eval-{i}") for i in range(4)]
for j in jobs:
    print(j["id"], j["state"])

for row in c.usage(by="job")["rows"]:
    print(f'{row["key"]:12} {row["gpu_hours"]:.4f} gpu-hours')
